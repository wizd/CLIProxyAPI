package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestParseDataURL(t *testing.T) {
	mimeType, data, ok := ParseDataURL("data:video/mp4;base64,AAAA")
	if !ok {
		t.Fatal("expected data URL to parse")
	}
	if mimeType != "video/mp4" || data != "AAAA" {
		t.Fatalf("got %q %q", mimeType, data)
	}
}

func TestIsYouTubeURL(t *testing.T) {
	cases := map[string]bool{
		"https://www.youtube.com/watch?v=jNQXAC9IVRw": true,
		"https://youtu.be/jNQXAC9IVRw":                true,
		"https://m.youtube.com/shorts/abc":            true,
		"https://example.com/watch?v=jNQXAC9IVRw":     false,
		"data:video/mp4;base64,AAAA":                  false,
	}
	for raw, want := range cases {
		if got := IsYouTubeURL(raw); got != want {
			t.Fatalf("IsYouTubeURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestVideoPartFromOpenAIItemDataURL(t *testing.T) {
	item := gjson.Parse(`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAIGZ0eXBtcDQy"}}`)
	part := VideoPartFromOpenAIItem(item, VideoPartGeminiSnake)
	if got := gjson.GetBytes(part, "inlineData.mime_type").String(); got != "video/mp4" {
		t.Fatalf("mime = %q", got)
	}
	if got := gjson.GetBytes(part, "inlineData.data").String(); got != "AAAAIGZ0eXBtcDQy" {
		t.Fatalf("data = %q", got)
	}
}

func TestVideoPartFromOpenAIItemYouTube(t *testing.T) {
	item := gjson.Parse(`{"type":"video_url","video_url":{"url":"https://www.youtube.com/watch?v=jNQXAC9IVRw","start_offset":5,"end_offset":"15s","fps":1}}`)
	part := VideoPartFromOpenAIItem(item, VideoPartGeminiCamel)
	if got := gjson.GetBytes(part, "fileData.fileUri").String(); got != "https://www.youtube.com/watch?v=jNQXAC9IVRw" {
		t.Fatalf("fileUri = %q", got)
	}
	if got := gjson.GetBytes(part, "videoMetadata.startOffset.seconds").Int(); got != 5 {
		t.Fatalf("startOffset = %d", got)
	}
	if got := gjson.GetBytes(part, "videoMetadata.endOffset.seconds").Int(); got != 15 {
		t.Fatalf("endOffset = %d", got)
	}
	if got := gjson.GetBytes(part, "videoMetadata.fps").Float(); got != 1 {
		t.Fatalf("fps = %v", got)
	}
}

func TestVideoPartFromOpenAIItemFileID(t *testing.T) {
	item := gjson.Parse(`{"type":"video_url","file_id":"file-0123456789abcdef","video_metadata":{"fps":1}}`)
	part := VideoPartFromOpenAIItem(item, VideoPartGeminiCamel)
	if got := gjson.GetBytes(part, "fileData.fileUri").String(); got != "gwfile://file-0123456789abcdef" {
		t.Fatalf("fileUri = %q part=%s", got, part)
	}
	if got := gjson.GetBytes(part, "videoMetadata.fps").Int(); got != 1 {
		t.Fatalf("fps = %v", gjson.GetBytes(part, "videoMetadata").Raw)
	}
}

func TestVideoPartFromURLGatewayFile(t *testing.T) {
	part := VideoPartFromURL("gwfile://file-abc12345", VideoPartResponses)
	if got := gjson.GetBytes(part, "file_data.file_uri").String(); got != "gwfile://file-abc12345" {
		t.Fatalf("file_uri = %q", got)
	}
}

func TestGatewayFileID(t *testing.T) {
	id, ok := GatewayFileID("gwfile://file-abc")
	if !ok || id != "file-abc" {
		t.Fatalf("got %q %v", id, ok)
	}
	if IsGatewayFileURI("https://example.com/x") {
		t.Fatal("http URL should not be a gateway file")
	}
}

func TestVideoPartFromOpenAIItemResponsesYouTube(t *testing.T) {
	item := gjson.Parse(`{"type":"input_video","video_url":"https://youtu.be/jNQXAC9IVRw"}`)
	part := VideoPartFromOpenAIItem(item, VideoPartResponses)
	if got := gjson.GetBytes(part, "file_data.file_uri").String(); got != "https://youtu.be/jNQXAC9IVRw" {
		t.Fatalf("file_uri = %q, part=%s", got, part)
	}
}
