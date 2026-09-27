package basispoints

import (
	"encoding/base64"
	"strings"
)

// ImageUploadFunc uploads one decoded image blob and returns the upstream
// openai_file_id. Implementations live in the gateway service (proxy-aware
// HTTP + account credential headers).
type ImageUploadFunc func(mediaType string, data []byte) (string, error)

// DecodeImageDataURL splits a data: URL into (media_type, bytes); returns nil
// for anything that is not a base64 data URL.
func DecodeImageDataURL(url string) (string, []byte) {
	header, payload, found := strings.Cut(url, ",")
	if !found || !strings.Contains(header, ";base64") {
		return "", nil
	}
	mediaType := strings.TrimPrefix(header, "data:")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil
	}
	return mediaType, data
}

// RewriteImages converts input_image parts carrying data: URLs into file_id
// form via upload. It rewrites the value tree in place for maps/slices it owns
// (callers pass the freshly built upstream input, so mutation is safe) and
// increments stats.Images for each attempted conversion.
//
// On failure the part degrades to an input_text note rather than dropping the
// item — the upstream body whitelist is strict and an opaque removal would
// confuse tool replay bookkeeping.
func RewriteImages(value any, upload ImageUploadFunc, stats *ImageStats) any {
	switch v := value.(type) {
	case []any:
		for i, item := range v {
			v[i] = RewriteImages(item, upload, stats)
		}
		return v
	case map[string]any:
		url, _ := v["image_url"].(string)
		if v["type"] == "input_image" && strings.HasPrefix(url, "data:") {
			if stats != nil {
				stats.Images++
			}
			mediaType, data := DecodeImageDataURL(url)
			if data == nil {
				return map[string]any{"type": "input_text", "text": "[image content omitted: could not decode data URL]"}
			}
			fileID, err := upload(mediaType, data)
			if err != nil {
				return map[string]any{"type": "input_text", "text": "[image content omitted: upload failed: " + err.Error() + "]"}
			}
			delete(v, "image_url")
			v["file_id"] = fileID
			if _, ok := v["detail"]; !ok {
				v["detail"] = "auto"
			}
			return v
		}
		for key, item := range v {
			v[key] = RewriteImages(item, upload, stats)
		}
		return v
	}
	return value
}

// ImageStats counts images converted by RewriteImages for request logging.
type ImageStats struct {
	Images int
}

// AttachmentsURL derives the attachment upload endpoint from the configured
// responses URL: https://bps.openai.com/basispoints/api/responses ->
// https://bps.openai.com/basispoints/api/attachments.
func AttachmentsURL(responsesURL string) string {
	if idx := strings.LastIndex(responsesURL, "/"); idx > 0 {
		return responsesURL[:idx] + "/attachments"
	}
	return strings.TrimSuffix(responsesURL, "/") + "/attachments"
}

// AttachmentFileName mirrors the reference bridge: picture-<digest12>.<ext>.
func AttachmentFileName(digest, mediaType string) string {
	exts := map[string]string{
		"image/png":  "png",
		"image/jpeg": "jpg",
		"image/gif":  "gif",
		"image/webp": "webp",
	}
	ext, ok := exts[mediaType]
	if !ok {
		ext = "png"
	}
	short := digest
	if len(short) > 12 {
		short = short[:12]
	}
	return "picture-" + short + "." + ext
}
