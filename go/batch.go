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

	"github.com/spf13/cobra"
)

const batchLong = `Submit and manage asynchronous image batches with the Gemini Batch API.

Each manifest line is {"key":"unique-name","request":{"contents":[...]}}.
The request may include any GenerateContentRequest fields, such as generationConfig.
Download saves batch-results.jsonl and extracts visible images and text.`

var batchResourceID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var batchModelID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// runBatch executes a batch subcommand line, such as "status batches/id", with this client.
func (c *geminiClient) runBatch(ctx context.Context, args []string, out io.Writer) error {
	cmd := newBatchCmd(func() (*geminiClient, error) {
		if strings.TrimSpace(c.apiKey) == "" {
			return nil, errors.New(missingAPIKeyMessage)
		}
		return c, nil
	})
	cmd.SetArgs(args)
	cmd.SetOut(out)
	return cmd.ExecuteContext(ctx)
}

// newBatchCmd builds the batch command tree. The client is requested only
// after a subcommand's arguments are valid, so usage mistakes never need an API key.
func newBatchCmd(client func() (*geminiClient, error)) *cobra.Command {
	batch := &cobra.Command{
		Use:                        "batch",
		Short:                      "Submit and manage asynchronous image batches",
		Long:                       batchLong,
		SilenceUsage:               true,
		SilenceErrors:              true,
		DisableFlagsInUseLine:      true,
		SuggestionsMinimumDistance: 2,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "help" {
				return nil
			}
			message := fmt.Sprintf("unknown batch command %q", args[0])
			if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
				message += "; did you mean " + strings.Join(suggestions, " or ") + "?"
			}
			return usageError{errors.New(message)}
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	// The root command's grouped generation help does not apply to this subtree.
	batch.SetUsageTemplate((&cobra.Command{}).UsageTemplate())
	batch.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })

	batchName := func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usagef("usage: %s", cmd.UseLine())
		}
		if _, err := validateBatchName(args[0]); err != nil {
			return usageError{err}
		}
		return nil
	}

	var model, displayName string
	submit := &cobra.Command{
		Use:                   "submit <manifest.jsonl> [--model <alias-or-id>] [--name <display-name>]",
		Short:                 "Upload a JSONL manifest and start a batch",
		DisableFlagsInUseLine: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("usage: %s", cmd.UseLine())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return c.batchSubmit(cmd.Context(), args[0], model, displayName, cmd.OutOrStdout())
		},
	}
	submit.Flags().StringVarP(&model, "model", "m", "flash", "Alias or full ID of the `model`")
	submit.Flags().StringVar(&displayName, "name", "", "Display `name` for the batch (default: the manifest file name)")

	status := &cobra.Command{
		Use:                   "status <batches/id>",
		Short:                 "Print a batch's status as JSON",
		Args:                  batchName,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			body, err := c.batchJSON(cmd.Context(), http.MethodGet, "/"+args[0], nil)
			if err != nil {
				return err
			}
			return printBatchJSON(cmd.OutOrStdout(), body)
		},
	}

	var outputDir string
	download := &cobra.Command{
		Use:                   "download <batches/id> [--output-dir <directory>]",
		Short:                 "Save a completed batch's results and extract its images and text",
		Args:                  batchName,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if outputDir == "" {
				return usagef("--output-dir requires a nonempty value")
			}
			c, err := client()
			if err != nil {
				return err
			}
			return c.batchDownload(cmd.Context(), args[0], outputDir, cmd.OutOrStdout())
		},
	}
	download.Flags().StringVar(&outputDir, "output-dir", ".", "Save results in this `directory`")

	cancel := &cobra.Command{
		Use:                   "cancel <batches/id>",
		Short:                 "Request cancellation of a running batch",
		Args:                  batchName,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if _, err := c.batchJSON(cmd.Context(), http.MethodPost, "/"+args[0]+":cancel", nil); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Cancellation requested: %s\n", args[0])
			return err
		},
	}
	batch.AddCommand(submit, status, download, cancel)
	return batch
}

func validateBatchName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "batches/")
	if !ok || !batchResourceID.MatchString(id) {
		return "", errors.New("batch name must have the form batches/id")
	}
	return name, nil
}

func (c *geminiClient) batchSubmit(ctx context.Context, path, model, name string, out io.Writer) error {
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if name == "" || name == "." {
		name = "generate-batch"
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

func (c *geminiClient) batchDownload(ctx context.Context, name, dir string, out io.Writer) error {
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
