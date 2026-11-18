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
	"strings"
	"time"
)

func interactionPending(status string) bool {
	return status == "in_progress" || status == "queued" || status == "pending"
}

func (c *geminiClient) getVideoInteraction(ctx context.Context, request videoGenerationRequest) (videoGenerationResult, error) {
	id := request.GetInteractionID
	if !omniFileID.MatchString(id) {
		return videoGenerationResult{}, errors.New("invalid Omni interaction ID")
	}
	if request.StreamProgress != nil {
		response, err := c.videoRequest(ctx, http.MethodGet, "/interactions/"+id+"?stream=true", nil, "", false)
		if err != nil {
			return videoGenerationResult{}, err
		}
		defer response.Body.Close()
		if err := c.videoStatusError(response); err != nil {
			return videoGenerationResult{}, err
		}
		interaction, raw, err := c.readVideoEvents(response.Body, request.StreamProgress, request.CaptureResponse)
		if err != nil {
			return videoGenerationResult{}, err
		}
		return c.videoResult(ctx, interaction, raw)
	}
	for {
		response, err := c.videoRequest(ctx, http.MethodGet, "/interactions/"+id, nil, "", false)
		if err != nil {
			return videoGenerationResult{}, err
		}
		if err := c.videoStatusError(response); err != nil {
			response.Body.Close()
			return videoGenerationResult{}, err
		}
		interaction, raw, err := readVideoInteraction(response.Body)
		response.Body.Close()
		if err != nil {
			return videoGenerationResult{}, err
		}
		if !interactionPending(interaction.Status) {
			return c.videoResult(ctx, interaction, raw)
		}
		select {
		case <-ctx.Done():
			return videoGenerationResult{}, ctx.Err()
		case <-time.After(mediaPollInterval):
		}
	}
}

func (c *geminiClient) cancelVideoInteraction(ctx context.Context, id string) (videoGenerationResult, error) {
	if !omniFileID.MatchString(id) {
		return videoGenerationResult{}, errors.New("invalid Omni interaction ID")
	}
	response, err := c.videoRequest(ctx, http.MethodPost, "/interactions/"+id+"/cancel", nil, "", false)
	if err != nil {
		return videoGenerationResult{}, err
	}
	defer response.Body.Close()
	if err := c.videoStatusError(response); err != nil {
		return videoGenerationResult{}, err
	}
	interaction, raw, err := readVideoInteraction(response.Body)
	if err != nil {
		return videoGenerationResult{}, err
	}
	return videoGenerationResult{InteractionID: id, Status: c.boundedDiagnostic(interaction.Status), RawResponse: raw}, nil
}

func (c *geminiClient) readVideoEvents(body io.Reader, progress func(string), capture bool) (omniResponse, []byte, error) {
	var result omniResponse
	var output []omniInput
	var events []json.RawMessage
	reader := bufio.NewReader(body)
	var data bytes.Buffer
	finished := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return result, nil, fmt.Errorf("read Omni video stream: %w", readErr)
		}
		line = bytes.TrimRight(line, "\r\n")
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
		if (len(line) == 0 || readErr == io.EOF) && data.Len() > 0 {
			payload := data.Bytes()
			if bytes.Equal(payload, []byte("[DONE]")) {
				break
			}
			var event struct {
				Type        string       `json:"event_type"`
				Status      string       `json:"status"`
				Interaction omniResponse `json:"interaction"`
				Delta       struct {
					omniInput
					Content omniInput `json:"content"`
				} `json:"delta"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				return result, nil, fmt.Errorf("decode Omni video stream event: %w", err)
			}
			if capture {
				events = append(events, bytes.Clone(payload))
			}
			data.Reset()
			switch event.Type {
			case "interaction.created":
				result.ID = event.Interaction.ID
				result.Status = event.Interaction.Status
				if result.ID != "" {
					progress("Interaction ID: " + c.redact(result.ID))
				}
			case "interaction.status_update":
				result.Status = event.Status
			case "interaction.completed":
				finished = true
				if event.Interaction.ID == "" {
					event.Interaction.ID = result.ID
				}
				result = event.Interaction
			case "step.delta":
				switch event.Delta.Type {
				case "video", "text":
					output = append(output, event.Delta.omniInput)
				case "thought_summary":
					if summary := c.redact(event.Delta.Content.Text); summary != "" {
						progress(summary)
					}
				}
			case "error":
				return result, nil, fmt.Errorf("Omni video stream failed: %s", c.boundedDiagnostic(event.Error.Message))
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if result.Status == "" {
		return result, nil, errors.New("Omni video stream ended before reporting a status")
	}
	if result.Status == "completed" && !finished {
		return result, nil, errors.New("Omni video stream ended before the completion event")
	}
	if len(output) > 0 {
		var text strings.Builder
		parts := make([]omniInput, 0, len(output))
		for _, part := range output {
			if part.Type == "text" {
				text.WriteString(part.Text)
			} else {
				parts = append(parts, part)
			}
		}
		if text.Len() > 0 {
			parts = append(parts, omniInput{Type: "text", Text: text.String()})
		}
		result.Steps = append(result.Steps, omniStep{Type: "model_output", Content: parts})
	}
	if !capture {
		return result, nil, nil
	}
	raw, err := json.Marshal(events)
	return result, raw, err
}
