// Package compute — Document and structured data converter.
// Uses LibreOffice (soffice) or Pandoc when installed for rich layout fidelity,
// falling back to built-in pure-Go readers/writers (PDF 1.4, DOCX, ODT, EPUB,
// RTF, HTML, Markdown, TXT, JSON, CSV, XML, YAML) with zero external dependencies.
package compute

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var documentFormats = map[string]bool{
	"pdf": true, "docx": true, "doc": true, "odt": true, "epub": true,
	"rtf": true, "md": true, "markdown": true, "html": true, "htm": true,
	"txt": true, "csv": true, "json": true, "xml": true, "yaml": true, "yml": true,
	"pptx": true, "xlsx": true,
}

// IsDocumentFormat reports whether the format extension belongs to the document/data converter.
func IsDocumentFormat(ext string) bool {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	return documentFormats[ext]
}

// ConvertDocument converts documents/structured data using external tools (soffice/pandoc) when
// available for rich layout fidelity, and falls back to built-in pure-Go converters so
// txt/md/html/rtf/docx/odt/epub/pdf/json/csv/xml/yaml always work with zero external dependencies.
func ConvertDocument(ctx context.Context, inPath, outPath, inFormat, outFormat, title string) (string, error) {
	inFmt := normalizeDocFmt(inFormat)
	outFmt := normalizeDocFmt(outFormat)
	if title == "" {
		title = "Document"
	}

	// 1. Try LibreOffice (soffice) for rich Office <-> PDF conversions if installed.
	if (inFmt == "docx" || inFmt == "doc" || inFmt == "odt" || inFmt == "pptx" || inFmt == "xlsx" || inFmt == "rtf") &&
		(outFmt == "pdf" || outFmt == "docx" || outFmt == "odt" || outFmt == "html" || outFmt == "txt") {
		if soffice, err := exec.LookPath("soffice"); err == nil {
			outDir := filepath.Dir(outPath)
			cmd := command(ctx, soffice, "--headless", "--convert-to", outFmt, "--outdir", outDir, inPath)
			if cmd.Run() == nil {
				produced := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))+"."+outFmt)
				if _, statErr := os.Stat(produced); statErr == nil {
					if produced != outPath {
						_ = os.Rename(produced, outPath)
					}
					return "libreoffice", nil
				}
			}
		}
	}

	// 2. Try Pandoc for rich document conversions if installed (skip pdf unless pdf engine is present).
	if outFmt != "pdf" && outFmt != "csv" && outFmt != "json" && outFmt != "xml" && outFmt != "yaml" {
		if pandoc, err := exec.LookPath("pandoc"); err == nil {
			cmd := command(ctx, pandoc, inPath, "-o", outPath)
			if cmd.Run() == nil {
				if _, statErr := os.Stat(outPath); statErr == nil {
					return "pandoc", nil
				}
			}
		}
	}

	// 3. Built-in Pure-Go Document & Data Engine (zero external dependencies).
	rawBytes, err := os.ReadFile(inPath)
	if err != nil {
		return "", err
	}

	// Structured data conversions (JSON <-> CSV <-> XML <-> YAML <-> HTML)
	if isDataFmt(inFmt) && isDataFmt(outFmt) {
		outBytes, err := convertStructuredData(rawBytes, inFmt, outFmt)
		if err == nil {
			return "ourchive-data", os.WriteFile(outPath, outBytes, 0o644)
		}
	}

	plainText, err := extractDocumentText(rawBytes, inFmt)
	if err != nil {
		return "", err
	}

	var outBytes []byte
	switch outFmt {
	case "txt":
		outBytes = []byte(plainText)
	case "md":
		outBytes = []byte(plainText)
	case "html":
		outBytes = renderHTMLDocument(title, plainText, inFmt == "md")
	case "rtf":
		outBytes = renderRTFDocument(plainText)
	case "docx":
		outBytes, err = renderDOCXDocument(title, plainText)
	case "odt":
		outBytes, err = renderODTDocument(title, plainText)
	case "epub":
		outBytes, err = renderEPUBDocument(title, plainText)
	case "pdf":
		outBytes = renderPDFDocument(title, plainText)
	case "json":
		outBytes, err = json.MarshalIndent(map[string]any{
			"title":   title,
			"lines":   strings.Split(strings.ReplaceAll(plainText, "\r\n", "\n"), "\n"),
			"content": plainText,
		}, "", "  ")
	case "csv":
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{"line", "text"})
		for i, line := range strings.Split(strings.ReplaceAll(plainText, "\r\n", "\n"), "\n") {
			_ = w.Write([]string{fmt.Sprintf("%d", i+1), line})
		}
		w.Flush()
		outBytes = buf.Bytes()
	case "xml":
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<document>\n")
		sb.WriteString("  <title>" + html.EscapeString(title) + "</title>\n")
		sb.WriteString("  <body>" + html.EscapeString(plainText) + "</body>\n</document>\n")
		outBytes = []byte(sb.String())
	case "yaml":
		var sb strings.Builder
		sb.WriteString("title: " + strconvQuote(title) + "\ncontent: |\n")
		for _, line := range strings.Split(strings.ReplaceAll(plainText, "\r\n", "\n"), "\n") {
			sb.WriteString("  " + line + "\n")
		}
		outBytes = []byte(sb.String())
	default:
		return "", fmt.Errorf("unsupported document target format %q", outFmt)
	}
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(outPath, outBytes, 0o644); err != nil {
		return "", err
	}
	return "ourchive-doc", nil
}

func normalizeDocFmt(f string) string {
	f = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "."))
	switch f {
	case "markdown":
		return "md"
	case "htm":
		return "html"
	case "yml":
		return "yaml"
	default:
		return f
	}
}

func isDataFmt(f string) bool {
	return f == "json" || f == "csv" || f == "xml" || f == "yaml"
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func convertStructuredData(raw []byte, inFmt, outFmt string) ([]byte, error) {
	var rows []map[string]any
	switch inFmt {
	case "json":
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err == nil {
			rows = arr
		} else {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, err
			}
			rows = []map[string]any{obj}
		}
	case "csv":
		r := csv.NewReader(bytes.NewReader(raw))
		recs, err := r.ReadAll()
		if err != nil || len(recs) == 0 {
			return nil, errors.New("invalid csv")
		}
		headers := recs[0]
		for _, rec := range recs[1:] {
			m := map[string]any{}
			for i, h := range headers {
				if i < len(rec) {
					m[h] = rec[i]
				} else {
					m[h] = ""
				}
			}
			rows = append(rows, m)
		}
	default:
		return nil, errors.New("fallback to text")
	}

	switch outFmt {
	case "json":
		return json.MarshalIndent(rows, "", "  ")
	case "csv":
		keySet := map[string]bool{}
		for _, r := range rows {
			for k := range r {
				keySet[k] = true
			}
		}
		keys := make([]string, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write(keys)
		for _, r := range rows {
			rec := make([]string, len(keys))
			for i, k := range keys {
				if v, ok := r[k]; ok && v != nil {
					rec[i] = fmt.Sprint(v)
				}
			}
			_ = w.Write(rec)
		}
		w.Flush()
		return buf.Bytes(), nil
	case "xml":
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<records>\n")
		for _, r := range rows {
			sb.WriteString("  <record>\n")
			keys := make([]string, 0, len(r))
			for k := range r {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				tag := sanitizeXMLTag(k)
				sb.WriteString(fmt.Sprintf("    <%s>%s</%s>\n", tag, html.EscapeString(fmt.Sprint(r[k])), tag))
			}
			sb.WriteString("  </record>\n")
		}
		sb.WriteString("</records>\n")
		return []byte(sb.String()), nil
	case "yaml":
		var sb strings.Builder
		for _, r := range rows {
			keys := make([]string, 0, len(r))
			for k := range r {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				prefix := "  "
				if i == 0 {
					prefix = "- "
				}
				sb.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, k, strconvQuote(fmt.Sprint(r[k]))))
			}
		}
		return []byte(sb.String()), nil
	}
	return nil, errors.New("unsupported data target")
}

var xmlTagCleanRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeXMLTag(s string) string {
	s = xmlTagCleanRe.ReplaceAllString(strings.TrimSpace(s), "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return "field_" + s
	}
	return s
}

var (
	htmlScriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	htmlBlockTagRe    = regexp.MustCompile(`(?i)</?(p|div|br|h[1-6]|li|tr|blockquote|hr)[^>]*>`)
	htmlAnyTagRe      = regexp.MustCompile(`<[^>]+>`)
	rtfCtrlRe         = regexp.MustCompile(`\\[a-zA-Z]+-?\d* ?|\\'[0-9a-fA-F]{2}|[{}]`)
)

func extractDocumentText(raw []byte, inFmt string) (string, error) {
	switch inFmt {
	case "docx":
		return extractXMLFromZip(raw, "word/document.xml", "p")
	case "odt":
		return extractXMLFromZip(raw, "content.xml", "p")
	case "epub":
		return extractEPUBText(raw)
	case "pdf":
		return extractPDFText(raw), nil
	case "html":
		s := htmlScriptStyleRe.ReplaceAllString(string(raw), "")
		s = htmlBlockTagRe.ReplaceAllString(s, "\n")
		s = htmlAnyTagRe.ReplaceAllString(s, "")
		return strings.TrimSpace(html.UnescapeString(s)), nil
	case "rtf":
		s := strings.ReplaceAll(string(raw), `\par`, "\n")
		s = strings.ReplaceAll(s, `\line`, "\n")
		s = rtfCtrlRe.ReplaceAllString(s, "")
		return strings.TrimSpace(s), nil
	default:
		return strings.TrimSpace(string(raw)), nil
	}
}

func extractXMLFromZip(raw []byte, targetFile, paraLocalName string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("invalid %s archive: %w", targetFile, err)
	}
	for _, f := range zr.File {
		if f.Name != targetFile {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		dec := xml.NewDecoder(rc)
		var sb strings.Builder
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return strings.TrimSpace(sb.String()), nil
			}
			switch t := tok.(type) {
			case xml.CharData:
				sb.Write(t)
			case xml.EndElement:
				if t.Name.Local == paraLocalName || t.Name.Local == "br" {
					sb.WriteByte('\n')
				}
			}
		}
		return strings.TrimSpace(sb.String()), nil
	}
	return "", fmt.Errorf("missing %s inside archive", targetFile)
}

func extractEPUBText(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("invalid epub archive: %w", err)
	}
	var parts []string
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		if !strings.HasSuffix(lower, ".xhtml") && !strings.HasSuffix(lower, ".html") && !strings.HasSuffix(lower, ".htm") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
		_ = rc.Close()
		txt, _ := extractDocumentText(data, "html")
		if txt != "" {
			parts = append(parts, txt)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

var (
	pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	pdfTextOpRe = regexp.MustCompile(`\(([^()\\]*(?:\\.[^()\\]*)*)\)\s*Tj`)
)

func extractPDFText(raw []byte) string {
	var out []string
	scanChunk := func(chunk []byte) {
		matches := pdfTextOpRe.FindAllSubmatch(chunk, -1)
		for _, m := range matches {
			s := string(m[1])
			s = strings.ReplaceAll(s, `\(`, "(")
			s = strings.ReplaceAll(s, `\)`, ")")
			s = strings.ReplaceAll(s, `\\`, `\`)
			if strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	}
	for _, m := range pdfStreamRe.FindAllSubmatch(raw, -1) {
		streamData := m[1]
		if zr, err := zlib.NewReader(bytes.NewReader(streamData)); err == nil {
			if decoded, err := io.ReadAll(io.LimitReader(zr, 4<<20)); err == nil {
				scanChunk(decoded)
			}
			_ = zr.Close()
		} else {
			scanChunk(streamData)
		}
	}
	if len(out) == 0 {
		scanChunk(raw)
	}
	return strings.Join(out, "\n")
}

func renderHTMLDocument(title, body string, fromMarkdown bool) []byte {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var content strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if fromMarkdown {
			if strings.HasPrefix(trimmed, "### ") {
				content.WriteString("<h3>" + html.EscapeString(strings.TrimPrefix(trimmed, "### ")) + "</h3>\n")
				continue
			}
			if strings.HasPrefix(trimmed, "## ") {
				content.WriteString("<h2>" + html.EscapeString(strings.TrimPrefix(trimmed, "## ")) + "</h2>\n")
				continue
			}
			if strings.HasPrefix(trimmed, "# ") {
				content.WriteString("<h1>" + html.EscapeString(strings.TrimPrefix(trimmed, "# ")) + "</h1>\n")
				continue
			}
		}
		content.WriteString("<p>" + html.EscapeString(line) + "</p>\n")
	}
	return []byte(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:780px;margin:40px auto;padding:0 20px;line-height:1.65;color:#18181b}h1,h2,h3{line-height:1.25}p{margin:0 0 12px}</style>
</head>
<body>
%s
</body>
</html>
`, html.EscapeString(title), content.String()))
}

func renderRTFDocument(body string) []byte {
	var sb strings.Builder
	sb.WriteString(`{\rtf1\ansi\deff0 {\fonttbl {\f0 Helvetica;}}\f0\fs22 `)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		escaped := strings.ReplaceAll(line, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `{`, `\{`)
		escaped = strings.ReplaceAll(escaped, `}`, `\}`)
		sb.WriteString(escaped)
		sb.WriteString(`\par` + "\n")
	}
	sb.WriteString("}\n")
	return []byte(sb.String())
}

func renderDOCXDocument(title, body string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeEntry := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	if err := writeEntry("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`); err != nil {
		return nil, err
	}

	if err := writeEntry("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`); err != nil {
		return nil, err
	}

	var docXML strings.Builder
	docXML.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		docXML.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		docXML.WriteString(html.EscapeString(line))
		docXML.WriteString(`</w:t></w:r></w:p>`)
	}
	docXML.WriteString(`</w:body></w:document>`)

	if err := writeEntry("word/document.xml", docXML.String()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderODTDocument(title, body string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// OpenDocument spec requires uncompressed mimetype as first entry.
	mh := &zip.FileHeader{Name: "mimetype", Method: zip.Store}
	mw, err := zw.CreateHeader(mh)
	if err != nil {
		return nil, err
	}
	_, _ = mw.Write([]byte("application/vnd.oasis.opendocument.text"))

	writeEntry := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	if err := writeEntry("META-INF/manifest.xml", `<?xml version="1.0" encoding="UTF-8"?>
<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">
  <manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/>
  <manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>
</manifest:manifest>`); err != nil {
		return nil, err
	}

	var contentXML strings.Builder
	contentXML.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.2"><office:body><office:text>`)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		contentXML.WriteString(`<text:p>`)
		contentXML.WriteString(html.EscapeString(line))
		contentXML.WriteString(`</text:p>`)
	}
	contentXML.WriteString(`</office:text></office:body></office:document-content>`)

	if err := writeEntry("content.xml", contentXML.String()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderEPUBDocument(title, body string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	mh := &zip.FileHeader{Name: "mimetype", Method: zip.Store}
	mw, err := zw.CreateHeader(mh)
	if err != nil {
		return nil, err
	}
	_, _ = mw.Write([]byte("application/epub+zip"))

	writeEntry := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	if err := writeEntry("META-INF/container.xml", `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`); err != nil {
		return nil, err
	}

	if err := writeEntry("OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" unique-identifier="bookid" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="bookid">urn:ourchive:%d</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:language>en</dc:language>
    <meta property="dcterms:modified">%s</meta>
  </metadata>
  <manifest>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`, time.Now().Unix(), html.EscapeString(title), time.Now().UTC().Format("2006-01-02T15:04:05Z"))); err != nil {
		return nil, err
	}

	var ch strings.Builder
	ch.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>` + html.EscapeString(title) + `</title></head><body><h1>` + html.EscapeString(title) + `</h1>`)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		ch.WriteString("<p>" + html.EscapeString(line) + "</p>")
	}
	ch.WriteString(`</body></html>`)

	if err := writeEntry("OEBPS/chapter1.xhtml", ch.String()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderPDFDocument generates a clean, valid multi-page PDF 1.4 document with Helvetica text
// and accurate xref offsets so it opens in every PDF viewer without external dependencies.
func renderPDFDocument(title, body string) []byte {
	const maxCharsPerLine = 84
	const linesPerPage = 48

	rawLines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	wrapped := []string{title, ""}
	for _, rl := range rawLines {
		if len(rl) <= maxCharsPerLine {
			wrapped = append(wrapped, rl)
			continue
		}
		for len(rl) > maxCharsPerLine {
			wrapped = append(wrapped, rl[:maxCharsPerLine])
			rl = rl[maxCharsPerLine:]
		}
		wrapped = append(wrapped, rl)
	}

	var pages [][]string
	for i := 0; i < len(wrapped); i += linesPerPage {
		end := i + linesPerPage
		if end > len(wrapped) {
			end = len(wrapped)
		}
		pages = append(pages, wrapped[i:end])
	}
	if len(pages) == 0 {
		pages = [][]string{{title}}
	}

	escapePDF := func(s string) string {
		var sb strings.Builder
		for _, r := range s {
			if r == '\\' || r == '(' || r == ')' {
				sb.WriteByte('\\')
				sb.WriteRune(r)
			} else if r >= 32 && r <= 126 {
				sb.WriteRune(r)
			} else {
				sb.WriteByte(' ')
			}
		}
		return sb.String()
	}

	// Object numbers:
	// 1: Catalog, 2: Pages, 3: Font (Helvetica)
	// For each page i: (4 + 2*i) = Page, (5 + 2*i) = Content stream
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := []int{0} // object 0 is free

	addObj := func(objNum int, body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", objNum, body))
	}

	pageRefs := make([]string, len(pages))
	for i := range pages {
		pageRefs[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}

	addObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	addObj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(pageRefs, " "), len(pages)))
	addObj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i, pageLines := range pages {
		pageObj := 4 + 2*i
		contentObj := 5 + 2*i
		addObj(pageObj, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentObj))

		var stream strings.Builder
		stream.WriteString("BT\n/F1 11 Tf\n50 742 Td\n14 TL\n")
		for idx, line := range pageLines {
			if idx == 0 {
				stream.WriteString(fmt.Sprintf("(%s) Tj\n", escapePDF(line)))
			} else {
				stream.WriteString(fmt.Sprintf("T* (%s) Tj\n", escapePDF(line)))
			}
		}
		stream.WriteString("ET\n")
		s := stream.String()
		addObj(contentObj, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(s), s))
	}

	xrefPos := buf.Len()
	buf.WriteString(fmt.Sprintf("xref\n0 %d\n", len(offsets)))
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < len(offsets); i++ {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", offsets[i]))
	}
	buf.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefPos))
	return buf.Bytes()
}
