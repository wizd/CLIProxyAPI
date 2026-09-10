package common

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// VideoPartKind selects the Gemini JSON field naming used by a translator.
type VideoPartKind int

const (
	// VideoPartGeminiSnake writes inlineData.mime_type (Gemini Chat Completions).
	VideoPartGeminiSnake VideoPartKind = iota
	// VideoPartGeminiCamel writes inlineData.mimeType (Antigravity).
	VideoPartGeminiCamel
	// VideoPartResponses writes inline_data.mime_type (OpenAI Responses → Gemini).
	VideoPartResponses
)

// ParseDataURL extracts MIME type and payload from a data: URL.
func ParseDataURL(raw string) (mimeType, data string, ok bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 5 || !strings.EqualFold(raw[:5], "data:") {
		return "", "", false
	}
	rest := raw[5:]
	meta, payload, found := strings.Cut(rest, ",")
	if !found || payload == "" {
		return "", "", false
	}
	fields := strings.Split(meta, ";")
	mimeType = strings.TrimSpace(fields[0])
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	for _, field := range fields[1:] {
		if strings.EqualFold(strings.TrimSpace(field), "base64") {
			return mimeType, payload, true
		}
	}
	return mimeType, payload, true
}

// IsYouTubeURL reports whether raw is a YouTube watch/share URL that Gemini
// cloudcode-pa accepts as fileData.fileUri.
func IsYouTubeURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "youtu.be", "www.youtu.be", "youtube.com", "www.youtube.com", "m.youtube.com",
		"music.youtube.com", "youtube-nocookie.com", "www.youtube-nocookie.com":
		return true
	default:
		return strings.HasSuffix(host, ".youtube.com")
	}
}

// IsRemoteHTTPURL reports whether raw is an http(s) URL that is not a data URL.
func IsRemoteHTTPURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "data:") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return (scheme == "http" || scheme == "https") && parsed.Host != ""
}

// OpenAIVideoURL extracts a video URL from an OpenAI content part.
func OpenAIVideoURL(item gjson.Result) string {
	if url := strings.TrimSpace(item.Get("video_url.url").String()); url != "" {
		return url
	}
	if videoURL := item.Get("video_url"); videoURL.Type == gjson.String {
		if url := strings.TrimSpace(videoURL.String()); url != "" {
			return url
		}
	}
	if url := strings.TrimSpace(item.Get("url").String()); url != "" {
		return url
	}
	return ""
}

// VideoPartFromOpenAIItem converts an OpenAI video part into a Gemini part.
// Data URLs become inlineData; YouTube and other http(s) URLs become fileData.
func VideoPartFromOpenAIItem(item gjson.Result, kind VideoPartKind) []byte {
	videoURL := OpenAIVideoURL(item)
	part := VideoPartFromURL(videoURL, kind)
	if len(part) == 0 {
		return nil
	}
	return AttachVideoMetadata(part, VideoMetadataFromItem(item))
}

// VideoPartFromURL converts a video URL into a Gemini part without metadata.
func VideoPartFromURL(videoURL string, kind VideoPartKind) []byte {
	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		return nil
	}
	if mimeType, data, ok := ParseDataURL(videoURL); ok {
		return GeminiInlineDataPart(mimeType, data, kind)
	}
	if IsRemoteHTTPURL(videoURL) {
		mimeType := mimeFromVideoURL(videoURL)
		return GeminiFileDataPart(videoURL, mimeType, kind)
	}
	return nil
}

// GeminiInlineDataPart builds an inlineData / inline_data part.
func GeminiInlineDataPart(mimeType, data string, kind VideoPartKind) []byte {
	switch kind {
	case VideoPartResponses:
		part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inline_data.mime_type", mimeType)
		part, _ = sjson.SetBytes(part, "inline_data.data", data)
		return part
	case VideoPartGeminiCamel:
		part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
		part, _ = sjson.SetBytes(part, "inlineData.data", data)
		return part
	default:
		part := []byte(`{"inlineData":{"mime_type":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inlineData.mime_type", mimeType)
		part, _ = sjson.SetBytes(part, "inlineData.data", data)
		return part
	}
}

// GeminiFileDataPart builds a fileData / file_data part.
func GeminiFileDataPart(fileURI, mimeType string, kind VideoPartKind) []byte {
	if mimeType == "" {
		mimeType = "video/mp4"
	}
	switch kind {
	case VideoPartResponses:
		part := []byte(`{"file_data":{"mime_type":"","file_uri":""}}`)
		part, _ = sjson.SetBytes(part, "file_data.mime_type", mimeType)
		part, _ = sjson.SetBytes(part, "file_data.file_uri", fileURI)
		return part
	case VideoPartGeminiCamel:
		part := []byte(`{"fileData":{"mimeType":"","fileUri":""}}`)
		part, _ = sjson.SetBytes(part, "fileData.mimeType", mimeType)
		part, _ = sjson.SetBytes(part, "fileData.fileUri", fileURI)
		return part
	default:
		part := []byte(`{"fileData":{"mime_type":"","file_uri":""}}`)
		part, _ = sjson.SetBytes(part, "fileData.mime_type", mimeType)
		part, _ = sjson.SetBytes(part, "fileData.file_uri", fileURI)
		return part
	}
}

// VideoMetadataFromItem extracts Gemini videoMetadata from an OpenAI part.
func VideoMetadataFromItem(item gjson.Result) []byte {
	if raw := firstExistingObject(item, "video_metadata", "videoMetadata"); raw != "" {
		return normalizeVideoMetadata([]byte(raw))
	}
	if nested := item.Get("video_url"); nested.Exists() && nested.IsObject() {
		if raw := firstExistingObject(nested, "video_metadata", "videoMetadata"); raw != "" {
			return normalizeVideoMetadata([]byte(raw))
		}
		return videoMetadataFromOffsets(nested)
	}
	return videoMetadataFromOffsets(item)
}

// AttachVideoMetadata merges videoMetadata onto a Gemini part when present.
func AttachVideoMetadata(part, metadata []byte) []byte {
	if len(part) == 0 || len(metadata) == 0 || !gjson.ValidBytes(metadata) {
		return part
	}
	part, _ = sjson.SetRawBytes(part, "videoMetadata", metadata)
	return part
}

func firstExistingObject(item gjson.Result, paths ...string) string {
	for _, pathName := range paths {
		value := item.Get(pathName)
		if value.Exists() && value.IsObject() {
			return value.Raw
		}
	}
	return ""
}

func videoMetadataFromOffsets(item gjson.Result) []byte {
	start := firstOffset(item, "start_offset", "startOffset")
	end := firstOffset(item, "end_offset", "endOffset")
	fps := firstFPS(item)
	if start == "" && end == "" && fps == "" {
		return nil
	}
	out := []byte(`{}`)
	if start != "" {
		out, _ = sjson.SetRawBytes(out, "startOffset", []byte(start))
	}
	if end != "" {
		out, _ = sjson.SetRawBytes(out, "endOffset", []byte(end))
	}
	if fps != "" {
		out, _ = sjson.SetRawBytes(out, "fps", []byte(fps))
	}
	return out
}

func firstOffset(item gjson.Result, paths ...string) string {
	for _, pathName := range paths {
		value := item.Get(pathName)
		if !value.Exists() {
			continue
		}
		if encoded := encodeOffset(value); encoded != "" {
			return encoded
		}
	}
	return ""
}

func encodeOffset(value gjson.Result) string {
	if value.IsObject() {
		return value.Raw
	}
	switch value.Type {
	case gjson.Number:
		seconds := int64(value.Num)
		nanos := int64((value.Num - float64(seconds)) * 1e9)
		out := []byte(`{"seconds":0}`)
		out, _ = sjson.SetBytes(out, "seconds", seconds)
		if nanos != 0 {
			out, _ = sjson.SetBytes(out, "nanos", nanos)
		}
		return string(out)
	case gjson.String:
		seconds, ok := parseDurationSeconds(value.String())
		if !ok {
			return ""
		}
		out := []byte(`{"seconds":0}`)
		whole := int64(seconds)
		nanos := int64((seconds - float64(whole)) * 1e9)
		out, _ = sjson.SetBytes(out, "seconds", whole)
		if nanos != 0 {
			out, _ = sjson.SetBytes(out, "nanos", nanos)
		}
		return string(out)
	default:
		return ""
	}
}

func firstFPS(item gjson.Result) string {
	for _, pathName := range []string{"fps", "frame_rate"} {
		value := item.Get(pathName)
		if !value.Exists() {
			continue
		}
		switch value.Type {
		case gjson.Number:
			return value.Raw
		case gjson.String:
			if trimmed := strings.TrimSpace(value.String()); trimmed != "" {
				if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
					return trimmed
				}
			}
		}
	}
	return ""
}

func parseDurationSeconds(raw string) (float64, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	raw = strings.TrimSuffix(raw, "s")
	if raw == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return seconds, true
}

func normalizeVideoMetadata(raw []byte) []byte {
	if !gjson.ValidBytes(raw) {
		return nil
	}
	out := []byte(`{}`)
	wrote := false
	if start := encodeOffset(gjson.GetBytes(raw, "startOffset")); start == "" {
		start = encodeOffset(gjson.GetBytes(raw, "start_offset"))
		if start != "" {
			out, _ = sjson.SetRawBytes(out, "startOffset", []byte(start))
			wrote = true
		}
	} else {
		out, _ = sjson.SetRawBytes(out, "startOffset", []byte(start))
		wrote = true
	}
	if end := encodeOffset(gjson.GetBytes(raw, "endOffset")); end == "" {
		end = encodeOffset(gjson.GetBytes(raw, "end_offset"))
		if end != "" {
			out, _ = sjson.SetRawBytes(out, "endOffset", []byte(end))
			wrote = true
		}
	} else {
		out, _ = sjson.SetRawBytes(out, "endOffset", []byte(end))
		wrote = true
	}
	if fps := gjson.GetBytes(raw, "fps"); fps.Exists() {
		out, _ = sjson.SetRawBytes(out, "fps", []byte(fps.Raw))
		wrote = true
	} else if fps := gjson.GetBytes(raw, "frame_rate"); fps.Exists() {
		out, _ = sjson.SetRawBytes(out, "fps", []byte(fps.Raw))
		wrote = true
	}
	if !wrote {
		return raw
	}
	return out
}

func mimeFromVideoURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "video/mp4"
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(parsed.Path), "."))
	if ext == "" {
		return "video/mp4"
	}
	if mime := misc.MimeTypes[ext]; strings.HasPrefix(mime, "video/") {
		return mime
	}
	return "video/mp4"
}
