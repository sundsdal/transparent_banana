package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The inline limit leaves room for base64 expansion, prompts, and JSON in a
// 20 MiB request. Larger files use the reusable Files API.
const maxInlineMediaBytes = 14 << 20
const maxPDFBytes = 50_000_000
const mediaPollInterval = 2 * time.Second
const mediaProcessingTimeout = 10 * time.Minute

type geminiFileData struct {
	MIMEType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiFile struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	State    string `json:"state"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

// prepareImageInputs preserves input order. Video is always uploaded because
// the Files API must finish processing it before it can be used in a prompt.
func (c *geminiClient) prepareImageInputs(ctx context.Context, paths []string) ([]geminiPart, error) {
	parts := make([]geminiPart, 0, len(paths))
	inlineBytes := int64(0)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mimeType, isVideo, err := mediaMIME(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect input %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("input %q must be a nonempty regular file", path)
		}
		if mimeType == "application/pdf" && info.Size() > maxPDFBytes {
			return nil, fmt.Errorf("PDF %q exceeds the 50 MB Gemini limit", path)
		}
		if err := validateMediaHeader(path, mimeType); err != nil {
			return nil, err
		}
		if !isVideo && info.Size() <= maxInlineMediaBytes && inlineBytes+info.Size() <= maxInlineMediaBytes {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read input %q: %w", path, err)
			}
			parts = append(parts, geminiPart{InlineData: &geminiInlineData{MIMEType: mimeType, Data: base64.StdEncoding.EncodeToString(data)}})
			inlineBytes += info.Size()
			continue
		}
		file, err := c.uploadFile(ctx, path, mimeType)
		if err != nil {
			return nil, err
		}
		file, err = c.waitForFile(ctx, file)
		if err != nil {
			return nil, err
		}
		if file.URI == "" {
			return nil, fmt.Errorf("Gemini upload of %q returned no file URI", path)
		}
		if file.MIMEType == "" {
			file.MIMEType = mimeType
		}
		parts = append(parts, geminiPart{FileData: &geminiFileData{MIMEType: file.MIMEType, FileURI: file.URI}})
	}
	return parts, nil
}

func mediaMIME(path string) (string, bool, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", false, nil
	case ".jpg", ".jpeg":
		return "image/jpeg", false, nil
	case ".gif":
		return "image/gif", false, nil
	case ".webp":
		return "image/webp", false, nil
	case ".heic":
		return "image/heic", false, nil
	case ".heif":
		return "image/heif", false, nil
	case ".pdf":
		return "application/pdf", false, nil
	case ".mp4", ".m4v":
		return "video/mp4", true, nil
	case ".mpeg":
		return "video/mpeg", true, nil
	case ".mpg":
		return "video/mpg", true, nil
	case ".mov":
		return "video/mov", true, nil
	case ".avi":
		return "video/avi", true, nil
	case ".flv":
		return "video/x-flv", true, nil
	case ".webm":
		return "video/webm", true, nil
	case ".wmv":
		return "video/wmv", true, nil
	case ".3gp", ".3gpp":
		return "video/3gpp", true, nil
	default:
		return "", false, fmt.Errorf("unsupported input %q (use PNG, JPEG, GIF, WebP, HEIC, HEIF, PDF, or a supported video file)", path)
	}
}

func validateMediaHeader(path, mimeType string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open input %q: %w", path, err)
	}
	defer file.Close()
	var header [32]byte
	n, err := io.ReadFull(file, header[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read input %q: %w", path, err)
	}
	b := header[:n]
	valid := true
	switch mimeType {
	case "image/png":
		valid = bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		valid = bytes.HasPrefix(b, []byte("\xff\xd8\xff"))
	case "image/gif":
		valid = bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	case "image/webp":
		valid = len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	case "image/heic", "image/heif":
		valid = len(b) >= 12 && string(b[4:8]) == "ftyp" && isHEIFBrand(string(b[8:12]))
	case "application/pdf":
		valid = bytes.HasPrefix(b, []byte("%PDF-"))
	}
	if !valid {
		return fmt.Errorf("input %q is not a valid %s file", path, mimeType)
	}
	return nil
}

func isHEIFBrand(brand string) bool {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heis", "heim", "hevs", "hevm", "mif1", "msf1", "miaf":
		return true
	default:
		return false
	}
}

// uploadFile streams a local file through the Gemini resumable Files endpoint.
// Its MIME type is caller supplied so batch JSONL manifests can use this too.
func (c *geminiClient) uploadFile(ctx context.Context, path, mimeType string) (geminiFile, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return geminiFile{}, errors.New("Gemini API key is required")
	}
	if strings.TrimSpace(mimeType) == "" || strings.ContainsAny(mimeType, "\r\n") {
		return geminiFile{}, errors.New("upload MIME type is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return geminiFile{}, fmt.Errorf("open upload %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return geminiFile{}, fmt.Errorf("inspect upload %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return geminiFile{}, fmt.Errorf("upload %q must be a nonempty regular file", path)
	}
	base := c.baseURL
	if base == "" {
		base = defaultGeminiBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "https" && baseURL.Scheme != "http") {
		return geminiFile{}, errors.New("invalid Gemini API base URL")
	}
	startURL := *baseURL
	startURL.RawQuery = ""
	startURL.Fragment = ""
	pathPrefix := strings.TrimSuffix(strings.TrimRight(startURL.Path, "/"), "/v1beta")
	startURL.Path = pathPrefix + "/upload/v1beta/files"
	metadata, err := json.Marshal(struct {
		File struct {
			DisplayName string `json:"displayName"`
		} `json:"file"`
	}{File: struct {
		DisplayName string `json:"displayName"`
	}{DisplayName: filepath.Base(path)}})
	if err != nil {
		return geminiFile{}, fmt.Errorf("encode upload metadata: %w", err)
	}
	start, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL.String(), bytes.NewReader(metadata))
	if err != nil {
		return geminiFile{}, errors.New("create Gemini upload request failed")
	}
	start.Header.Set("x-goog-api-key", c.apiKey)
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("X-Goog-Upload-Protocol", "resumable")
	start.Header.Set("X-Goog-Upload-Command", "start")
	start.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(info.Size(), 10))
	start.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)
	response, err := c.doMediaRequest(start)
	if err != nil {
		return geminiFile{}, c.mediaTransportError(ctx, "start Gemini upload", err)
	}
	if err := c.mediaStatusError(response); err != nil {
		response.Body.Close()
		return geminiFile{}, err
	}
	uploadLocation := response.Header.Get("X-Goog-Upload-URL")
	response.Body.Close()
	uploadURL, err := url.Parse(uploadLocation)
	if err != nil || uploadURL.Scheme != baseURL.Scheme || uploadURL.Host != baseURL.Host || uploadURL.User != nil {
		return geminiFile{}, errors.New("Gemini returned an invalid upload URL")
	}
	if !strings.HasPrefix(uploadURL.Path, pathPrefix+"/upload/v1beta/") {
		return geminiFile{}, errors.New("Gemini returned an invalid upload URL")
	}
	upload, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), file)
	if err != nil {
		return geminiFile{}, errors.New("create Gemini upload transfer failed")
	}
	upload.ContentLength = info.Size()
	upload.Header.Set("Content-Type", mimeType)
	upload.Header.Set("X-Goog-Upload-Offset", "0")
	upload.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	response, err = c.doMediaRequest(upload)
	if err != nil {
		return geminiFile{}, c.mediaTransportError(ctx, "transfer Gemini upload", err)
	}
	defer response.Body.Close()
	if err := c.mediaStatusError(response); err != nil {
		return geminiFile{}, err
	}
	var result struct {
		File geminiFile `json:"file"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return geminiFile{}, fmt.Errorf("decode Gemini upload response: %w", err)
	}
	if result.File.Name == "" || result.File.URI == "" {
		return geminiFile{}, errors.New("Gemini upload response is missing file name or URI")
	}
	if !validGeminiFileName(result.File.Name) {
		return geminiFile{}, errors.New("Gemini upload response has an invalid file name")
	}
	if result.File.MIMEType == "" {
		result.File.MIMEType = mimeType
	}
	return result.File, nil
}

func (c *geminiClient) waitForFile(ctx context.Context, file geminiFile) (geminiFile, error) {
	ctx, cancel := context.WithTimeout(ctx, mediaProcessingTimeout)
	defer cancel()
	if !validGeminiFileName(file.Name) {
		return geminiFile{}, errors.New("Gemini returned an invalid file name")
	}
	for {
		switch file.State {
		case "ACTIVE":
			return file, nil
		case "FAILED":
			return geminiFile{}, fmt.Errorf("Gemini file processing failed: %s", c.boundedDiagnostic(file.Error.Message))
		case "", "PROCESSING", "STATE_UNSPECIFIED":
		default:
			return geminiFile{}, fmt.Errorf("Gemini file has unexpected state: %s", c.boundedDiagnostic(file.State))
		}
		if err := ctx.Err(); err != nil {
			return geminiFile{}, fmt.Errorf("wait for Gemini file: %w", err)
		}
		base := c.baseURL
		if base == "" {
			base = defaultGeminiBaseURL
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+file.Name, nil)
		if err != nil {
			return geminiFile{}, errors.New("create Gemini file status request failed")
		}
		request.Header.Set("x-goog-api-key", c.apiKey)
		response, err := c.doMediaRequest(request)
		if err != nil {
			return geminiFile{}, c.mediaTransportError(ctx, "get Gemini file status", err)
		}
		if err := c.mediaStatusError(response); err != nil {
			response.Body.Close()
			return geminiFile{}, err
		}
		var latest geminiFile
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&latest)
		response.Body.Close()
		if decodeErr != nil {
			return geminiFile{}, fmt.Errorf("decode Gemini file status: %w", decodeErr)
		}
		if latest.Name != "" && latest.Name != file.Name {
			return geminiFile{}, errors.New("Gemini file status returned a different file name")
		}
		if latest.Name == "" {
			latest.Name = file.Name
		}
		if latest.URI == "" {
			latest.URI = file.URI
		}
		if latest.MIMEType == "" {
			latest.MIMEType = file.MIMEType
		}
		file = latest
		if file.State == "ACTIVE" || file.State == "FAILED" {
			continue
		}
		timer := time.NewTimer(mediaPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return geminiFile{}, fmt.Errorf("wait for Gemini file: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func validGeminiFileName(name string) bool {
	if !strings.HasPrefix(name, "files/") {
		return false
	}
	id := strings.TrimPrefix(name, "files/")
	if id == "" || len(id) > 40 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func (c *geminiClient) doMediaRequest(request *http.Request) (*http.Response, error) {
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copy.Do(request)
}

func (c *geminiClient) mediaTransportError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", operation, ctx.Err())
	}
	// A transport error may contain the signed upload URL and API key.
	return fmt.Errorf("%s failed", operation)
}

func (c *geminiClient) mediaStatusError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	var apiErr geminiAPIError
	message := ""
	if json.Unmarshal(data, &apiErr) == nil {
		message = apiErr.Error.Message
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("Gemini Files API returned HTTP %d: %s", response.StatusCode, c.boundedDiagnostic(message))
}
