package compute

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ourchive/ourchive/pkg/protocol"
)

func TestDocumentConversionsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	sampleMD := "# Ourchive Report\n\nHello from Ourchive document converter.\nSecond line of content.\n"
	mdPath := filepath.Join(dir, "report.md")
	if err := os.WriteFile(mdPath, []byte(sampleMD), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := []struct {
		ext         string
		magicPrefix []byte
	}{
		{"pdf", []byte("%PDF-1.4")},
		{"docx", []byte("PK\x03\x04")},
		{"odt", []byte("PK\x03\x04")},
		{"epub", []byte("PK\x03\x04")},
		{"rtf", []byte(`{\rtf1`)},
		{"html", []byte("<!doctype html>")},
		{"txt", []byte("# Ourchive Report")},
		{"json", []byte("{")},
		{"csv", []byte("line,text")},
		{"xml", []byte("<?xml")},
		{"yaml", []byte("title:")},
	}

	ctx := context.Background()
	produced := map[string]string{}
	for _, tc := range targets {
		outPath := filepath.Join(dir, "report."+tc.ext)
		enc, err := ConvertDocument(ctx, mdPath, outPath, "md", tc.ext, "report")
		if err != nil {
			t.Fatalf("md -> %s failed: %v", tc.ext, err)
		}
		data, err := os.ReadFile(outPath)
		if err != nil || len(data) == 0 {
			t.Fatalf("md -> %s produced empty output", tc.ext)
		}
		if !bytes.HasPrefix(data, tc.magicPrefix) {
			t.Fatalf("md -> %s unexpected header (enc=%s): got %q", tc.ext, enc, string(data[:min(24, len(data))]))
		}
		produced[tc.ext] = outPath
	}

	// Verify round-trip extraction back to plain text preserves our content.
	for _, srcExt := range []string{"docx", "odt", "epub", "pdf", "rtf", "html"} {
		outTxt := filepath.Join(dir, "roundtrip_"+srcExt+".txt")
		if _, err := ConvertDocument(ctx, produced[srcExt], outTxt, srcExt, "txt", "roundtrip"); err != nil {
			t.Fatalf("%s -> txt roundtrip failed: %v", srcExt, err)
		}
		got, err := os.ReadFile(outTxt)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "Hello from Ourchive document converter") {
			t.Fatalf("%s -> txt lost body content: got %q", srcExt, string(got))
		}
	}

	// Verify structured JSON <-> CSV <-> XML <-> YAML conversions.
	jsonPath := filepath.Join(dir, "items.json")
	_ = os.WriteFile(jsonPath, []byte(`[{"name":"alpha","size":"10"},{"name":"beta","size":"20"}]`), 0o644)
	csvPath := filepath.Join(dir, "items.csv")
	if _, err := ConvertDocument(ctx, jsonPath, csvPath, "json", "csv", "items"); err != nil {
		t.Fatalf("json -> csv failed: %v", err)
	}
	csvBytes, _ := os.ReadFile(csvPath)
	if !strings.Contains(string(csvBytes), "alpha,10") || !strings.Contains(string(csvBytes), "beta,20") {
		t.Fatalf("unexpected csv output: %q", string(csvBytes))
	}
	backJSON := filepath.Join(dir, "items_back.json")
	if _, err := ConvertDocument(ctx, csvPath, backJSON, "csv", "json", "items"); err != nil {
		t.Fatalf("csv -> json failed: %v", err)
	}
	backBytes, _ := os.ReadFile(backJSON)
	if !strings.Contains(string(backBytes), `"alpha"`) {
		t.Fatalf("unexpected csv->json output: %q", string(backBytes))
	}
}

func TestConvertRunnerAllMediaAndDocFormats(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed on test runner")
	}

	dir := t.TempDir()
	// Generate tiny 1-second synthetic video+audio MP4 and PNG image using ffmpeg lavfi.
	sampleMP4 := filepath.Join(dir, "sample.mp4")
	if out, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", sampleMP4,
	).CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize test mp4: %v (%s)", err, string(out))
	}
	samplePNG := filepath.Join(dir, "sample.png")
	if out, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=blue:s=128x128:d=0.1",
		"-frames:v", "1", samplePNG,
	).CombinedOutput(); err != nil {
		t.Fatalf("failed to synthesize test png: %v (%s)", err, string(out))
	}

	mp4Bytes, _ := os.ReadFile(sampleMP4)
	pngBytes, _ := os.ReadFile(samplePNG)

	runner := NewConvertRunner(protocol.ComputeCapabilities{
		Enabled:       true,
		FFmpegPath:    ffmpegPath,
		MaxConcurrent: 2,
	})

	runHTTP := func(filename string, body []byte, targetFmt, preset string) {
		t.Helper()
		url := "/compute/convert?format=" + targetFmt + "&filename=" + filename
		if preset != "" {
			url += "&preset=" + preset
		}
		req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		runner.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %s (preset=%s) failed HTTP %d: %s", filename, targetFmt, preset, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s -> %s returned empty body", filename, targetFmt)
		}
	}

	// Video formats
	for _, fmtName := range []string{"mp4", "mkv", "mov", "avi", "webm", "gif"} {
		runHTTP("sample.mp4", mp4Bytes, fmtName, "web_fast")
	}
	runHTTP("sample.mp4", mp4Bytes, "mp4", "discord_10mb")

	// Audio formats
	for _, fmtName := range []string{"mp3", "wav", "flac", "ogg", "opus", "m4a", "aac", "aiff"} {
		runHTTP("sample.mp4", mp4Bytes, fmtName, "")
	}

	// Image formats
	for _, fmtName := range []string{"webp", "jpg", "png", "bmp", "tiff", "ico"} {
		runHTTP("sample.png", pngBytes, fmtName, "")
	}

	// Document formats via HTTP runner
	for _, fmtName := range []string{"pdf", "docx", "odt", "epub", "rtf", "html"} {
		runHTTP("notes.md", []byte("# Notes\nTesting HTTP document conversion."), fmtName, "")
	}
}
