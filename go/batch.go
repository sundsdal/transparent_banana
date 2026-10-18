package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const batchHelp = `Usage:
  generate batch submit <manifest.jsonl> [--model <alias-or-id>] [--name <display-name>]
  generate batch status <batches/id>
  generate batch download <batches/id> [--output-dir <directory>]
  generate batch cancel <batches/id>

Each manifest line is {"key":"unique-name","request":{"contents":[...]}}.
The request may include any GenerateContentRequest fields, such as generationConfig.
Download saves batch-results.jsonl and extracts visible images and text.
`

var batchResourceID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var batchModelID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (c *geminiClient) runBatch(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := io.WriteString(out, batchHelp)
		return err
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return errors.New("Gemini API key is required")
	}
	switch args[0] {
	case "submit":
		return c.batchSubmit(ctx, args[1:], out)
	case "status":
		if len(args) != 2 {
			return errors.New("usage: generate batch status <batches/id>")
		}
		name, err := validateBatchName(args[1])
		if err != nil {
			return err
		}
		body, err := c.batchJSON(ctx, http.MethodGet, "/"+name, nil)
		if err != nil {
			return err
		}
		return printBatchJSON(out, body)
	case "cancel":
		if len(args) != 2 {
			return errors.New("usage: generate batch cancel <batches/id>")
		}
		name, err := validateBatchName(args[1])
		if err != nil {
			return err
		}
		if _, err = c.batchJSON(ctx, http.MethodPost, "/"+name+":cancel", nil); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Cancellation requested: %s\n", name)
		return err
	case "download":
		return c.batchDownload(ctx, args[1:], out)
	default:
		return fmt.Errorf("unknown batch command %q\n%s", args[0], batchHelp)
	}
}

func validateBatchName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "batches/")
	if !ok || !batchResourceID.MatchString(id) {
		return "", errors.New("batch name must have the form batches/id")
	}
	return name, nil
}

func (c *geminiClient) batchSubmit(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: generate batch submit <manifest.jsonl> [--model <alias-or-id>] [--name <display-name>]")
	}
	path := args[0]
	model := defaultModel
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if name == "" || name == "." {
		name = "generate-batch"
	}
	for i := 1; i < len(args); i++ {
		if i+1 >= len(args) {
			return fmt.Errorf("missing value for %s", args[i])
		}
		switch args[i] {
		case "--model", "-m":
			model = args[i+1]
		case "--name":
			name = args[i+1]
		default:
			return fmt.Errorf("unknown batch submit option %q", args[i])
		}
		i++
	}
	if resolved, ok := models[model]; ok {
		model = resolved
	}
	if !batchModelID.MatchString(model) || strings.TrimSpace(name) == "" {
		return errors.New("invalid batch model or display name")
	}
	if err := validateBatchManifest(path, model); err != nil {
		return err
	}
	file, err := c.uploadFile(ctx, path, "application/jsonl")
	if err != nil {
		return fmt.Errorf("upload batch manifest: %w", err)
	}
	file, err = c.waitForFile(ctx, file)
	if err != nil {
		return fmt.Errorf("wait for batch manifest: %w", err)
	}
	if !strings.HasPrefix(file.Name, "files/") || !batchResourceID.MatchString(strings.TrimPrefix(file.Name, "files/")) {
		return errors.New("Gemini returned an invalid uploaded file name")
	}
	payload, err := json.Marshal(map[string]any{"batch": map[string]any{
		"displayName": name, "inputConfig": map[string]string{"fileName": file.Name},
	}})
	if err != nil {
		return err
	}
	body, err := c.batchJSON(ctx, http.MethodPost, "/models/"+url.PathEscape(model)+":batchGenerateContent", payload)
	if err != nil {
		return err
	}
	var job struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Errorf("decode batch creation response: %w", err)
	}
	if _, err := validateBatchName(job.Name); err != nil {
		return fmt.Errorf("Gemini returned an invalid batch name: %w", err)
	}
	_, err = fmt.Fprintf(out, "Submitted: %s\n", job.Name)
	return err
}

func validateBatchManifest(path, model string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open batch manifest: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > 2_000_000_000 {
		return errors.New("batch manifest exceeds the 2 GB Files API limit")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	keys := map[string]bool{}
	line := 0
	for scanner.Scan() {
		line++
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			return fmt.Errorf("batch manifest line %d is empty", line)
		}
		var entry struct {
			Key     string          `json:"key"`
			Request json.RawMessage `json:"request"`
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return fmt.Errorf("batch manifest line %d: %w", line, err)
		}
		if entry.Key == "" || keys[entry.Key] {
			return fmt.Errorf("batch manifest line %d has a missing or duplicate key", line)
		}
		keys[entry.Key] = true
		var request struct {
			Model    string            `json:"model"`
			Contents []json.RawMessage `json:"contents"`
		}
		if err := json.Unmarshal(entry.Request, &request); err != nil || len(request.Contents) == 0 {
			return fmt.Errorf("batch manifest line %d needs a request with contents", line)
		}
		if request.Model != "" && request.Model != model && request.Model != "models/"+model {
			return fmt.Errorf("batch manifest line %d model differs from --model", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read batch manifest: %w", err)
	}
	if line == 0 {
		return errors.New("batch manifest is empty")
	}
	return nil
}

func (c *geminiClient) batchJSON(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	response, err := c.videoRequest(ctx, method, path, reader, "application/json", false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := c.videoStatusError(response); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("read Gemini batch response: %w", err)
	}
	if len(data) > 64*1024*1024 {
		return nil, errors.New("Gemini batch response exceeds 64 MB")
	}
	return data, nil
}

func printBatchJSON(out io.Writer, body []byte) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		return fmt.Errorf("decode Gemini batch response: %w", err)
	}
	pretty.WriteByte('\n')
	_, err := out.Write(pretty.Bytes())
	return err
}

func (c *geminiClient) batchDownload(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: generate batch download <batches/id> [--output-dir <directory>]")
	}
	name, err := validateBatchName(args[0])
	if err != nil {
		return err
	}
	dir := "."
	if len(args) > 1 {
		if len(args) != 3 || args[1] != "--output-dir" || args[2] == "" {
			return errors.New("usage: generate batch download <batches/id> [--output-dir <directory>]")
		}
		dir = args[2]
	}
	body, err := c.batchJSON(ctx, http.MethodGet, "/"+name, nil)
	if err != nil {
		return err
	}
	var job struct {
		Done     bool   `json:"done"`
		State    string `json:"state"`
		Metadata struct {
			State string `json:"state"`
		} `json:"metadata"`
		Response struct {
			ResponsesFile string `json:"responsesFile"`
			Output        struct {
				ResponsesFile    string          `json:"responsesFile"`
				InlinedResponses json.RawMessage `json:"inlinedResponses"`
			} `json:"output"`
			InlinedResponses json.RawMessage `json:"inlinedResponses"`
			Dest             struct {
				FileName         string          `json:"fileName"`
				InlinedResponses json.RawMessage `json:"inlinedResponses"`
			} `json:"dest"`
		} `json:"response"`
		Output struct {
			ResponsesFile    string          `json:"responsesFile"`
			InlinedResponses json.RawMessage `json:"inlinedResponses"`
		} `json:"output"`
		Dest struct {
			FileName         string          `json:"fileName"`
			InlinedResponses json.RawMessage `json:"inlinedResponses"`
		} `json:"dest"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Errorf("decode batch status: %w", err)
	}
	state := job.Metadata.State
	if state == "" {
		state = job.State
	}
	if !job.Done && state != "JOB_STATE_SUCCEEDED" && state != "BATCH_STATE_SUCCEEDED" {
		return fmt.Errorf("batch %s is not complete (state: %s)", name, state)
	}
	fileName := firstNonempty(job.Response.ResponsesFile, job.Response.Output.ResponsesFile, job.Response.Dest.FileName, job.Output.ResponsesFile, job.Dest.FileName)
	inline := firstNonemptyRaw(job.Response.InlinedResponses, job.Response.Output.InlinedResponses, job.Response.Dest.InlinedResponses, job.Output.InlinedResponses, job.Dest.InlinedResponses)
	if fileName == "" && len(inline) == 0 {
		return fmt.Errorf("batch %s has no downloadable results", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create batch output directory: %w", err)
	}
	rawPath := filepath.Join(dir, "batch-results.jsonl")
	if fileName != "" {
		if err := c.downloadBatchFile(ctx, fileName, rawPath); err != nil {
			return err
		}
	} else if err := writeInlineBatchResults(rawPath, inline); err != nil {
		return err
	}
	images, texts, failures, err := extractBatchResults(rawPath, dir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Saved: %s (%d images, %d text files, %d request errors)\n", rawPath, images, texts, failures)
	return err
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func firstNonemptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

func (c *geminiClient) downloadBatchFile(ctx context.Context, name, output string) error {
	id, ok := strings.CutPrefix(name, "files/")
	if !ok || !batchResourceID.MatchString(id) {
		return errors.New("Gemini returned an invalid batch results file name")
	}
	base := c.baseURL
	if base == "" {
		base = defaultGeminiBaseURL
	}
	if !strings.HasSuffix(strings.TrimRight(base, "/"), "/v1beta") {
		return errors.New("invalid Gemini batch API base URL")
	}
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1beta")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/download/v1beta/files/"+url.PathEscape(id)+":download?alt=media", nil)
	if err != nil {
		return err
	}
	request.Header.Set("x-goog-api-key", c.apiKey)
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	requestClient := *client
	requestClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many batch download redirects")
		}
		if next.URL.Scheme != via[0].URL.Scheme || next.URL.Host != via[0].URL.Host {
			if next.URL.Scheme != "https" {
				return errors.New("batch download redirected to a non-HTTPS origin")
			}
			next.Header.Del("x-goog-api-key")
		}
		return nil
	}
	response, err := requestClient.Do(request)
	if err != nil {
		return c.mediaTransportError(ctx, "download Gemini batch results", err)
	}
	defer response.Body.Close()
	if err := c.videoStatusError(response); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(output), ".batch-results-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		return fmt.Errorf("save Gemini batch results: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), output)
}

func writeInlineBatchResults(path string, raw json.RawMessage) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		var wrapper struct {
			InlinedResponses []json.RawMessage `json:"inlinedResponses"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return fmt.Errorf("decode inline batch results: %w", err)
		}
		entries = wrapper.InlinedResponses
	}
	var data bytes.Buffer
	for _, entry := range entries {
		data.Write(entry)
		data.WriteByte('\n')
	}
	return atomicWriteFile(path, data.Bytes(), 0o600)
}

func extractBatchResults(path, dir string) (images, texts, failures int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for line := 1; ; line++ {
		record, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return images, texts, failures, fmt.Errorf("read batch results: %w", readErr)
		}
		if len(record) == 0 && readErr == io.EOF {
			break
		}
		data := bytes.TrimSpace(record)
		if len(data) == 0 {
			continue
		}
		var entry struct {
			Key      string `json:"key"`
			Metadata struct {
				Key string `json:"key"`
			} `json:"metadata"`
			Response       json.RawMessage `json:"response"`
			Error          json.RawMessage `json:"error"`
			Candidates     json.RawMessage `json:"candidates"`
			PromptFeedback json.RawMessage `json:"promptFeedback"`
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return images, texts, failures, fmt.Errorf("batch result line %d: %w", line, err)
		}
		if len(entry.Error) > 0 && string(entry.Error) != "null" {
			failures++
			continue
		}
		responseBody := entry.Response
		if len(responseBody) == 0 && (len(entry.Candidates) > 0 || len(entry.PromptFeedback) > 0) {
			responseBody = append(json.RawMessage(nil), data...)
		}
		if len(responseBody) == 0 {
			failures++
			continue
		}
		result, err := decodeImageResponse(responseBody)
		if err != nil {
			return images, texts, failures, fmt.Errorf("batch result line %d: %w", line, err)
		}
		if len(result.Images) == 0 && !result.hasVisibleText() {
			failures++
			continue
		}
		key := entry.Key
		if key == "" {
			key = entry.Metadata.Key
		}
		key = safeBatchKey(key)
		prefix := fmt.Sprintf("%04d-%s", line, key)
		if err := saveImageResult(filepath.Join(dir, prefix+".png"), result, "", false); err != nil {
			return images, texts, failures, err
		}
		images += len(result.Images)
		if result.hasVisibleText() {
			texts++
		}
	}
	return images, texts, failures, nil
}

func safeBatchKey(key string) string {
	if key == "" {
		return "request"
	}
	var b strings.Builder
	for _, r := range key {
		if b.Len() >= 64 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
