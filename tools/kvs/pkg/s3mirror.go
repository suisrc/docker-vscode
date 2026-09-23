package pkg

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// progressReader wraps an io.Reader and reports read progress via onProgress.
type progressReader struct {
	r          *bytes.Reader
	total      int64
	written    int64
	onProgress func(written, total int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.written += int64(n)
	if pr.onProgress != nil && n > 0 {
		pr.onProgress(pr.written, pr.total)
	}
	return n, err
}

// s3UploadFile uploads a local file to an S3-compatible endpoint.
// onProgress (if non-nil) is called periodically with bytes uploaded and total.
func s3UploadFile(mc mirrorConfig, s3URL, localPath, contentType string, onProgress func(written, total int64)) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return s3UploadBytes(mc, s3URL, data, contentType, onProgress)
}

// s3UploadBytes uploads data to an S3-compatible endpoint using AWS Signature V4.
// onProgress (if non-nil) is called periodically with bytes uploaded and total.
func s3UploadBytes(mc mirrorConfig, s3URL string, data []byte, contentType string, onProgress func(written, total int64)) error {
	u, err := url.Parse(s3URL)
	if err != nil {
		return fmt.Errorf("parse S3 URL: %w", err)
	}

	host := u.Host
	objectKey := u.Path
	region := mc.S3Region
	if region == "" {
		region = "us-east-1"
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	payloadHash := sha256Hex(data)

	// Canonical request.
	// Content-Type must be included in the signature if it's sent as a header.
	canonicalURI := s3EncodePath(objectKey)
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentType, host, payloadHash, amzDate)
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("PUT\n%s\n%s\n%s\n%s\n%s",
		canonicalURI, u.RawQuery, canonicalHeaders, signedHeaders, payloadHash)

	// String to sign.
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, credentialScope, sha256Hex([]byte(canonicalRequest)))

	// Signing key + signature.
	signingKey := s3SignKey(mc.S3Secret, dateStamp, region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		mc.S3Access, credentialScope, signedHeaders, signature)

	// HTTP request.
	body := io.Reader(bytes.NewReader(data))
	if onProgress != nil {
		body = &progressReader{r: bytes.NewReader(data), total: int64(len(data)), onProgress: onProgress}
	}
	req, err := http.NewRequest("PUT", s3URL, body)
	if err != nil {
		return err
	}
	req.Host = host
	req.ContentLength = int64(len(data))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", authorization)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 upload failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// s3SignKey derives the AWS SigV4 signing key.
func s3SignKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hex returns the hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// s3EncodePath URI-encodes each path segment but keeps slashes.
// For S3 SigV4, the canonical URI must be the URI-encoded path where
// each path segment is encoded but "/" separators are preserved.
func s3EncodePath(path string) string {
	if path == "" {
		return "/"
	}
	var sb strings.Builder
	// Handle leading slash.
	if path[0] == '/' {
		sb.WriteByte('/')
		path = path[1:]
	}
	for i, seg := range strings.Split(path, "/") {
		if i > 0 {
			sb.WriteByte('/')
		}
		sb.WriteString(url.PathEscape(seg))
	}
	return sb.String()
}

// ==============================================================================
// syncto — generic local <-> S3 file sync subcommand
//
// Environment-only configuration (no kvs.ini required):
//
//	KVS_S3_PREFIX  base URL, e.g. https://oss.example.com  (required)
//	KVS_S3_ACCESS  access key ID                           (required)
//	KVS_S3_SECRET  secret access key                       (required)
//	KVS_S3_REGION  region (default us-east-1)
//
// Usage:
//
//	kvs syncto <source> s3:<target>   upload: local path → S3 object prefix
//	kvs syncto s3:<source> <target>   download: S3 object prefix → local path
//
// source/target may be a single file or a directory; a directory syncs
// recursively (directories map 1:1 onto object key prefixes, same-name
// files overwrite). Two S3 endpoints are NOT supported — download to
// local first.

// synctoConfig is the env-derived S3 config for the syncto subcommand.
type synctoConfig struct {
	base   string // KVS_S3_PREFIX (no trailing slash)
	access string
	secret string
	region string
}

// loadSynctoConfig reads the KVS_S3_* environment variables. This command
// deliberately ignores kvs.ini: it fails hard when a required variable is
// missing.
func loadSynctoConfig() synctoConfig {
	mc := synctoConfig{
		base:   strings.TrimRight(os.Getenv("KVS_S3_PREFIX"), "/"),
		access: os.Getenv("KVS_S3_ACCESS"),
		secret: os.Getenv("KVS_S3_SECRET"),
		region: os.Getenv("KVS_S3_REGION"),
	}
	var missing []string
	if mc.base == "" {
		missing = append(missing, "KVS_S3_PREFIX")
	}
	if mc.access == "" {
		missing = append(missing, "KVS_S3_ACCESS")
	}
	if mc.secret == "" {
		missing = append(missing, "KVS_S3_SECRET")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "syncto: missing required environment variables: %s\n"+
			"  export KVS_S3_PREFIX=https://oss.example.com\n"+
			"  export KVS_S3_ACCESS=<access key id>\n"+
			"  export KVS_S3_SECRET=<secret access key>\n"+
			"  export KVS_S3_REGION=<region>            (optional, default us-east-1)\n", strings.Join(missing, ", "))
		os.Exit(1)
	}
	if mc.region == "" {
		mc.region = "us-east-1"
	}
	return mc
}

// SynctoCommand implements `kvs syncto <source> <target>`.
// Exactly one of source/target starts with "s3:"; the other side is local.
func SynctoCommand(args []string) {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: kvs syncto <source> <target>\n"+
			"  kvs syncto /local/path s3:/bucket/key    upload local → S3\n"+
			"  kvs syncto s3:/bucket/key /local/path    download S3 → local\n"+
			"env: KVS_S3_PREFIX, KVS_S3_ACCESS, KVS_S3_SECRET [KVS_S3_REGION]\n")
		os.Exit(1)
	}
	src, dst := args[0], args[1]
	srcS3 := strings.HasPrefix(src, "s3:")
	dstS3 := strings.HasPrefix(dst, "s3:")
	if srcS3 == dstS3 {
		fmt.Fprintf(os.Stderr, "syncto: exactly one of source/target must be an s3: address (two S3 endpoints not supported; copy via local)\n")
		os.Exit(1)
	}
	mc := loadSynctoConfig()

	if dstS3 {
		// Upload: local file or directory → S3 prefix.
		info, err := os.Stat(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "syncto: local source %q: %v\n", src, err)
			os.Exit(1)
		}
		objectBase := strings.TrimPrefix(dst, "s3:")
		if info.IsDir() {
			if err := synctoUploadDir(mc, src, objectBase); err != nil {
				fmt.Fprintf(os.Stderr, "syncto: upload: %v\n", err)
				os.Exit(1)
			}
		} else {
			key := objectBase
			if strings.HasSuffix(key, "/") || filepath.Base(src) != filepath.Base(key) {
				key = strings.TrimSuffix(key, "/") + "/" + filepath.Base(src)
			}
			if err := synctoUploadFile(mc, src, key); err != nil {
				fmt.Fprintf(os.Stderr, "syncto: upload: %v\n", err)
				os.Exit(1)
			}
		}
		log.Printf("[syncto] upload done: %s → %s/%s", src, mc.base, strings.TrimPrefix(objectBase, "/"))
		return
	}

	// Download: S3 prefix → local file or directory.
	key := strings.TrimPrefix(src, "s3:")
	if err := synctoDownload(mc, key, dst); err != nil {
		fmt.Fprintf(os.Stderr, "syncto: download: %v\n", err)
		os.Exit(1)
	}
	log.Printf("[syncto] download done: %s/%s → %s", mc.base, strings.TrimPrefix(key, "/"), dst)
}

// synctoUploadFile PUTs one local file to the S3 object key.
func synctoUploadFile(mc synctoConfig, localPath, key string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", localPath)
	}
	s3URL := mc.base + "/" + strings.TrimPrefix(key, "/")
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	log.Printf("[syncto] PUT %s (%d bytes)", s3URL, len(data))
	return s3UploadBytes(s3mc(mc), s3URL, data, synctoContentType(key), nil)
}

// s3mc adapts synctoConfig onto the mirrorConfig used by s3UploadBytes.
func s3mc(mc synctoConfig) mirrorConfig {
	return mirrorConfig{S3Prefix: mc.base, S3Access: mc.access, S3Secret: mc.secret, S3Region: mc.region}
}

// synctoContentType guesses a Content-Type from the object key extension.
func synctoContentType(key string) string {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	case ".wasm":
		return "application/wasm"
	case ".zip":
		return "application/zip"
	case ".gz", ".tgz":
		return "application/gzip"
	case ".tar":
		return "application/x-tar"
	case ".mp4":
		return "video/mp4"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".sh":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// synctoUploadDir walks the local directory and uploads every file under it,
// mapping the relative path onto the S3 object key prefix.
func synctoUploadDir(mc synctoConfig, localDir, keyPrefix string) error {
	return filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		key := strings.Trim(keyPrefix, "/") + "/" + filepath.ToSlash(rel)
		return synctoUploadFile(mc, path, key)
	})
}

// synctoDownload copies an S3 object (or every object under a key prefix)
// to the local target path. A trailing "/" on the key, or an existing local
// directory target, forces prefix (directory) mode.
func synctoDownload(mc synctoConfig, key, localPath string) error {
	key = strings.TrimPrefix(key, "/")
	isDirHint := strings.HasSuffix(key, "/")
	key = strings.TrimSuffix(key, "/")

	// Try a single object first unless the caller hinted at a prefix.
	if !isDirHint {
		exists, err := s3ObjectExists(mc, key)
		if err != nil {
			return err
		}
		if exists {
			return synctoDownloadObject(mc, key, localPath)
		}
	}

	// Prefix (directory) mode: list and download all objects under key/.
	return synctoDownloadDir(mc, key, localPath)
}

// synctoDownloadObject GETs one S3 object and writes it to localPath.
func synctoDownloadObject(mc synctoConfig, key, localPath string) error {
	s3URL := mc.base + "/" + s3EncodePath("/"+key)
	req, err := http.NewRequest(http.MethodGet, s3URL, nil)
	if err != nil {
		return err
	}
	if err := s3SignRequest(mc, req, ""); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("s3 object not found: %s", s3URL)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: HTTP %d: %s", s3URL, resp.StatusCode, string(body))
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	log.Printf("[syncto] GET %s → %s (%d bytes)", s3URL, localPath, len(data))
	return os.WriteFile(localPath, data, 0o644)
}

// synctoDownloadDir lists objects under key/ and mirrors them locally.
func synctoDownloadDir(mc synctoConfig, key, localPath string) error {
	prefix := strings.Trim(key, "/") + "/"
	keys, err := s3ListKeys(mc, prefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("no objects under s3:%s", prefix)
	}
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		dst := filepath.Join(localPath, filepath.FromSlash(rel))
		if err := synctoDownloadObject(mc, k, dst); err != nil {
			return err
		}
	}
	return nil
}

// ==============================================================================
// minimal S3 client primitives (GET/HEAD/list), shared with the mirror command

// s3SignRequest signs an arbitrary HTTP request (AWS SigV4, UNSIGNED-PAYLOAD
// for GET/HEAD). contentType must match the Content-Type header actually sent
// ("" for GET/HEAD requests).
func s3SignRequest(mc synctoConfig, req *http.Request, contentType string) error {
	u := req.URL
	host := u.Host
	region := mc.region
	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	if contentType != "" {
		canonicalHeaders = fmt.Sprintf("content-type:%s\n", contentType) + canonicalHeaders
		signedHeaders = "content-type;" + signedHeaders
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		s3EncodePath(u.EscapedPath()),
		u.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credentialScope, sha256Hex([]byte(canonicalRequest)))
	signingKey := s3SignKey(mc.secret, dateStamp, region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		mc.access, credentialScope, signedHeaders, signature))
	return nil
}

// s3ObjectExists HEADs a single object.
func s3ObjectExists(mc synctoConfig, key string) (bool, error) {
	s3URL := mc.base + "/" + s3EncodePath("/"+key)
	req, err := http.NewRequest(http.MethodHead, s3URL, nil)
	if err != nil {
		return false, err
	}
	if err := s3SignRequest(mc, req, ""); err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: HTTP %d", s3URL, resp.StatusCode)
	}
}

// s3ListKeys returns all object keys under the given prefix (ListObjectsV2,
// paginated, key-ascending).
func s3ListKeys(mc synctoConfig, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		s3URL := mc.base + "/?" + q.Encode()
		req, err := http.NewRequest(http.MethodGet, s3URL, nil)
		if err != nil {
			return nil, err
		}
		if err := s3SignRequest(mc, req, ""); err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list %s: HTTP %d: %s", mc.base, resp.StatusCode, string(body))
		}
		var list struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse list XML: %w", err)
		}
		for _, c := range list.Contents {
			keys = append(keys, c.Key)
		}
		if !list.IsTruncated || list.NextContinuationToken == "" {
			sort.Strings(keys)
			return keys, nil
		}
		token = list.NextContinuationToken
	}
}
