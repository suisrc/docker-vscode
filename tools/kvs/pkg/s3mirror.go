package pkg

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
