package clientcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Download is a file being saved from the active hub to disk.
type Download struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Path      string    `json:"path"`
	Bytes     int64     `json:"bytes"`
	Total     int64     `json:"total"`
	State     string    `json:"state"` // running | done | failed
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

const maxDownloadsKept = 20

// maxBlobBytes caps /_client/save-blob uploads from the webview (converter output and the like).
const maxBlobBytes = 4 << 30

// safeFilename keeps a suggested download name to a plain base name.
func safeFilename(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." || name == "/" {
		return "download"
	}
	return name
}

// validHubPath reports whether p is a hub path the UI may download from through the app.
func validHubPath(p string) bool {
	return (strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/downloads/")) &&
		!strings.Contains(p, "://") && !strings.Contains(p, "\\")
}

// StartDownload asks where to save, then streams hubPath from the active hub to that file in the
// background. It returns nil, nil if the user cancels the dialog.
func (c *Core) StartDownload(hubPath, filename string) (*Download, error) {
	if !validHubPath(hubPath) {
		return nil, errors.New("downloads must come from the hub API")
	}
	hub, ok := c.hubs.Active()
	if !ok {
		return nil, errors.New("no hub selected")
	}
	dest, err := c.shell.SaveFileDialog(safeFilename(filename))
	if err != nil || dest == "" {
		return nil, err
	}

	c.mu.Lock()
	c.nextDLID++
	d := &Download{ID: strconv.Itoa(c.nextDLID), Filename: filepath.Base(dest), Path: dest, State: "running", StartedAt: time.Now().UTC()}
	c.downloads = append(c.downloads, d)
	if len(c.downloads) > maxDownloadsKept {
		c.downloads = c.downloads[len(c.downloads)-maxDownloadsKept:]
	}
	cp := *d
	c.mu.Unlock()
	c.changed()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		err := c.runDownload(hub, hubPath, d)
		c.mu.Lock()
		if err != nil {
			d.State, d.Error = "failed", err.Error()
		} else {
			d.State = "done"
		}
		c.mu.Unlock()
		c.changed()
		if err != nil {
			c.shell.Notify("Download failed", d.Filename+": "+err.Error())
		} else {
			c.shell.Notify("Download complete", d.Filename)
		}
	}()
	return &cp, nil
}

func (c *Core) runDownload(hub Hub, hubPath string, d *Download) error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, hub.URL+hubPath, nil)
	if err != nil {
		return err
	}
	if token, ok := c.hubs.Session(hub.ID); ok {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	}
	client := *c.http
	client.Timeout = 0 // large files; cancellation comes from c.ctx
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %s", resp.Status)
	}
	c.mu.Lock()
	d.Total = resp.ContentLength
	c.mu.Unlock()

	var written atomic.Int64
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				c.mu.Lock()
				d.Bytes = written.Load()
				c.mu.Unlock()
				c.changed()
			}
		}
	}()
	err = writeFileAtomic(d.Path, &countingReader{r: resp.Body, n: &written})
	close(stop)
	c.mu.Lock()
	d.Bytes = written.Load()
	c.mu.Unlock()
	return err
}

// SaveBlob asks where to save, then writes body there. Used for files the UI produced itself
// (blob: URLs), which the app's webview cannot hand to the OS on every platform.
func (c *Core) SaveBlob(ctx context.Context, filename string, body io.Reader) (string, error) {
	dest, err := c.shell.SaveFileDialog(safeFilename(filename))
	if err != nil || dest == "" {
		return "", err
	}
	if err := writeFileAtomic(dest, io.LimitReader(body, maxBlobBytes)); err != nil {
		return "", err
	}
	return dest, nil
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n.Add(int64(n))
	return n, err
}

// writeFileAtomic writes to dest via a temporary file in the same directory so a failed or
// cancelled download never leaves a truncated file under the chosen name.
func writeFileAtomic(dest string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
