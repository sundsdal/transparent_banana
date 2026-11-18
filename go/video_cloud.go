package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var vertexProjectID = regexp.MustCompile(`^(?:[a-z][a-z0-9-]*[a-z0-9]|[0-9]+)$`)
var gcsBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)

func vertexAccessToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_ACCESS_TOKEN")); token != "" {
		return token, nil
	}
	data, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token", "--quiet").Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("could not get a Google Cloud access token; sign in with gcloud auth login or set GOOGLE_CLOUD_ACCESS_TOKEN")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("gcloud returned an empty access token")
	}
	return token, nil
}

func splitGCSURI(uri string) (bucket, object string, err error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "gs" || parsed.User != nil || !gcsBucketName.MatchString(parsed.Host) ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", fmt.Errorf("invalid Cloud Storage URI %q", uri)
	}
	return parsed.Host, strings.TrimPrefix(parsed.Path, "/"), nil
}

func (c *geminiClient) openCloudVideo(ctx context.Context, uri string) (io.ReadCloser, error) {
	bucket, object, err := splitGCSURI(uri)
	if err != nil || object == "" {
		return nil, errors.New("Google Cloud returned an invalid video object URI")
	}
	endpoint := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(object) + "?alt=media"
	response, err := c.sendVideoRequest(ctx, http.MethodGet, endpoint, nil, "", false)
	if err != nil {
		return nil, err
	}
	if err := c.videoStatusError(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response.Body, nil
}
