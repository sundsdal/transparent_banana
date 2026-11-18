package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

type omniLocalMedia struct {
	path     string
	mimeType string
	isVideo  bool
	size     int64
	uri      string
}

func omniMediaMIME(path string) (string, bool, error) {
	if strings.HasPrefix(path, "gs://") {
		_, object, err := splitGCSURI(path)
		if err != nil {
			return "", false, err
		}
		path = object
	}
	return mediaMIME(path)
}

// prepareOmniInputs preserves the order of local images and videos. Images
// share a 14 MiB inline budget; videos and larger images use the Files API.
// Validate every input before uploading anything, so a later invalid path
// cannot leave an earlier file uploaded unnecessarily.
func (c *geminiClient) prepareOmniInputs(ctx context.Context, paths []string) ([]omniInput, error) {
	media := make([]omniLocalMedia, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mimeType, isVideo, err := omniMediaMIME(path)
		if err != nil {
			return nil, fmt.Errorf("unsupported Omni input %q (use PNG, JPEG, GIF, WebP, HEIC, HEIF, or a supported video file)", path)
		}
		if mimeType == "application/pdf" {
			return nil, fmt.Errorf("Omni input %q must be an image or video", path)
		}
		if strings.HasPrefix(path, "gs://") {
			if c.vertexProject == "" {
				return nil, fmt.Errorf("Cloud Storage input %q requires --vertex-project", path)
			}
			media = append(media, omniLocalMedia{path: path, mimeType: mimeType, isVideo: isVideo, uri: path})
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect Omni input %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("Omni input %q must be a nonempty regular file", path)
		}
		if err := validateMediaHeader(path, mimeType); err != nil {
			return nil, err
		}
		media = append(media, omniLocalMedia{path: path, mimeType: mimeType, isVideo: isVideo, size: info.Size()})
	}

	parts := make([]omniInput, 0, len(media))
	var inlineBytes int64
	for _, item := range media {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kind := "image"
		if item.isVideo {
			kind = "video"
		}
		if c.vertexProject != "" {
			switch item.mimeType {
			case "video/mov":
				item.mimeType = "video/quicktime"
			case "video/wmv":
				item.mimeType = "video/x-ms-wmv"
			}
		}
		if item.uri != "" {
			parts = append(parts, omniInput{Type: kind, URI: item.uri, MIMEType: item.mimeType})
			continue
		}
		if (!item.isVideo || c.vertexProject != "") && item.size <= maxInlineMediaBytes-inlineBytes {
			data, err := os.ReadFile(item.path)
			if err != nil {
				return nil, fmt.Errorf("read Omni input %q: %w", item.path, err)
			}
			if len(data) == 0 {
				return nil, fmt.Errorf("Omni input %q must be a nonempty regular file", item.path)
			}
			if int64(len(data)) <= maxInlineMediaBytes-inlineBytes {
				parts = append(parts, omniInput{Type: kind, Data: base64.StdEncoding.EncodeToString(data), MIMEType: item.mimeType})
				inlineBytes += int64(len(data))
				continue
			}
		}
		if c.vertexProject != "" {
			return nil, fmt.Errorf("Omni inputs exceed the inline media budget; use a gs:// URI for %q", item.path)
		}
		file, err := c.uploadFile(ctx, item.path, item.mimeType)
		if err != nil {
			return nil, err
		}
		file, err = c.waitForFile(ctx, file)
		if err != nil {
			return nil, err
		}
		if file.URI == "" {
			return nil, fmt.Errorf("Gemini upload of %q returned no file URI", item.path)
		}
		if file.MIMEType != "" && file.MIMEType != item.mimeType {
			return nil, fmt.Errorf("Gemini upload of %q returned unexpected MIME type %q", item.path, file.MIMEType)
		}
		parts = append(parts, omniInput{Type: kind, URI: file.URI, MIMEType: item.mimeType})
	}
	return parts, nil
}
