package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ourchive/ourchive/internal/settings"
)

// EdgeProvider abstracts opt-in external edge/object caches (S3-compatible R2/B2/MinIO/AWS or Bunny.net Storage).
type EdgeProvider interface {
	Put(ctx context.Context, objectKey, contentType string, body io.Reader, size int64) (publicURL string, err error)
	Delete(ctx context.Context, objectKey string) error
	TestConnection(ctx context.Context) error
}

func NewEdgeProvider(cfg settings.EdgeStorageConfig) (EdgeProvider, error) {
	if !cfg.Enabled || cfg.Provider == "" || cfg.Provider == "none" {
		return nil, errors.New("edge storage is disabled")
	}
	switch cfg.Provider {
	case "bunny":
		return &bunnyProvider{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}, nil
	case "s3":
		return &s3Provider{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("unsupported edge storage provider: %s", cfg.Provider)
	}
}

// --- Bunny.net Storage + Pull Zone ---

type bunnyProvider struct {
	cfg    settings.EdgeStorageConfig
	client *http.Client
}

func (b *bunnyProvider) storageURL(objectKey string) string {
	host := strings.TrimSpace(b.cfg.Endpoint)
	if host == "" {
		region := strings.TrimSpace(strings.ToLower(b.cfg.Region))
		if region != "" && region != "de" && region != "default" {
			host = region + ".storage.bunnycdn.com"
		} else {
			host = "storage.bunnycdn.com"
		}
	}
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimRight(host, "/")
	zone := strings.Trim(b.cfg.Bucket, "/")
	key := strings.TrimLeft(objectKey, "/")
	return fmt.Sprintf("https://%s/%s/%s", host, zone, key)
}

func (b *bunnyProvider) Put(ctx context.Context, objectKey, contentType string, body io.Reader, size int64) (string, error) {
	if b.cfg.Bucket == "" || b.cfg.SecretKey == "" {
		return "", errors.New("bunny storage zone and API key are required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.storageURL(objectKey), body)
	if err != nil {
		return "", err
	}
	req.Header.Set("AccessKey", b.cfg.SecretKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("bunny upload failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	pub := strings.TrimRight(b.cfg.PublicBaseURL, "/")
	if pub == "" {
		return b.storageURL(objectKey), nil
	}
	return pub + "/" + strings.TrimLeft(objectKey, "/"), nil
}

func (b *bunnyProvider) Delete(ctx context.Context, objectKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.storageURL(objectKey), nil)
	if err != nil {
		return err
	}
	req.Header.Set("AccessKey", b.cfg.SecretKey)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *bunnyProvider) TestConnection(ctx context.Context) error {
	testKey := ".ourchive-healthcheck.txt"
	if _, err := b.Put(ctx, testKey, "text/plain", bytes.NewReader([]byte("ok")), 2); err != nil {
		return err
	}
	_ = b.Delete(ctx, testKey)
	return nil
}

// --- S3-Compatible (Cloudflare R2, Backblaze B2, MinIO, Wasabi, AWS S3) ---

type s3Provider struct {
	cfg    settings.EdgeStorageConfig
	client *http.Client
}

func (s *s3Provider) objectURL(objectKey string) (string, error) {
	ep := strings.TrimRight(strings.TrimSpace(s.cfg.Endpoint), "/")
	if ep == "" || s.cfg.Bucket == "" {
		return "", errors.New("S3 endpoint and bucket are required")
	}
	if !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
		ep = "https://" + ep
	}
	return fmt.Sprintf("%s/%s/%s", ep, strings.Trim(s.cfg.Bucket, "/"), strings.TrimLeft(objectKey, "/")), nil
}

func (s *s3Provider) signAndDo(ctx context.Context, method, objectKey, contentType string, payload []byte) (*http.Response, error) {
	if s.cfg.AccessKey == "" || s.cfg.SecretKey == "" {
		return nil, errors.New("S3 access_key and secret_key are required")
	}
	rawURL, err := s.objectURL(objectKey)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	region := strings.TrimSpace(s.cfg.Region)
	if region == "" {
		region = "auto"
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(payload)

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Host", u.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", u.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("%s\n%s\n\n%s\n%s\n%s", method, u.EscapedPath(), canonicalHeaders, signedHeaders, payloadHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, sha256Hex([]byte(canonicalRequest)))

	signingKey := deriveS3Key(s.cfg.SecretKey, dateStamp, region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)

	return s.client.Do(req)
}

func (s *s3Provider) Put(ctx context.Context, objectKey, contentType string, body io.Reader, size int64) (string, error) {
	payload, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	resp, err := s.signAndDo(ctx, http.MethodPut, objectKey, contentType, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("S3 upload failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	pub := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if pub == "" {
		return s.objectURL(objectKey)
	}
	return pub + "/" + strings.TrimLeft(objectKey, "/"), nil
}

func (s *s3Provider) Delete(ctx context.Context, objectKey string) error {
	resp, err := s.signAndDo(ctx, http.MethodDelete, objectKey, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (s *s3Provider) TestConnection(ctx context.Context) error {
	testKey := ".ourchive-healthcheck.txt"
	if _, err := s.Put(ctx, testKey, "text/plain", bytes.NewReader([]byte("ok")), 2); err != nil {
		return err
	}
	_ = s.Delete(ctx, testKey)
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func deriveS3Key(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}
