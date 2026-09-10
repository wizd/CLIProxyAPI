package helps

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/upload"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestResolveRemoteVideoURLsLeavesYouTube(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"https://www.youtube.com/watch?v=jNQXAC9IVRw","mimeType":"video/mp4"}}]}]}}`)
	out, release, err := ResolveRemoteVideoURLs(context.Background(), &config.Config{}, payload, nil)
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.fileData.fileUri").String(); !strings.Contains(got, "youtube.com") {
		t.Fatalf("YouTube URI changed: %s", out)
	}
}

func TestResolveRemoteVideoURLsRejectsRemoteWhenDisabled(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"file_uri":"https://example.com/clip.mp4","mime_type":"video/mp4"}}]}]}`)
	_, release, err := ResolveRemoteVideoURLs(context.Background(), &config.Config{}, payload, nil)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("expected error when fetch-remote-urls is disabled")
	}
}

func TestResolveRemoteVideoURLsFetchesWhenEnabled(t *testing.T) {
	skipRemoteVideoSSRFForTest = true
	t.Cleanup(func() { skipRemoteVideoSSRFForTest = false })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/webm")
		_, _ = w.Write([]byte("webm-bytes"))
	}))
	t.Cleanup(server.Close)

	payload := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"` + server.URL + `/clip.webm","mimeType":"video/mp4"},"videoMetadata":{"fps":1}}]}]}`)
	cfg := &config.Config{Video: config.VideoConfig{FetchRemoteURLs: true}}
	out, release, err := ResolveRemoteVideoURLs(context.Background(), cfg, payload, nil)
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.mimeType").String(); got != "video/webm" {
		t.Fatalf("mimeType = %q. out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.data").String(); got != base64.StdEncoding.EncodeToString([]byte("webm-bytes")) {
		t.Fatalf("data = %q", got)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.videoMetadata.fps").Int(); got != 1 {
		t.Fatalf("metadata lost: %s", out)
	}
}

func TestResolveRemoteVideoURLsBlocksPrivateHost(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"http://127.0.0.1/secret.mp4"}}]}]}`)
	cfg := &config.Config{Video: config.VideoConfig{FetchRemoteURLs: true}}
	_, release, err := ResolveRemoteVideoURLs(context.Background(), cfg, payload, nil)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("expected private host to be rejected")
	}
}

func TestResolveRemoteVideoURLsExpandsGatewayFile(t *testing.T) {
	store, err := upload.Open(t.TempDir(), config.FileStoreConfig{TTL: "1h", MaxUploadMB: 1, MaxTotalGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	prev := upload.Current()
	upload.SetCurrent(store)
	t.Cleanup(func() { upload.SetCurrent(prev) })

	meta, err := store.Put("alice", strings.NewReader("clip-bytes"), "video/webm", "clip.webm")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"gwfile://` + meta.ID + `","mimeType":"video/mp4"},"videoMetadata":{"fps":2}}]}]}`)
	out, release, err := ResolveRemoteVideoURLs(context.Background(), &config.Config{}, payload, map[string]any{
		coreexecutor.CallerScopeMetadataKey: "alice",
	})
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.mimeType").String(); got != "video/webm" {
		t.Fatalf("mimeType = %q out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.inlineData.data").String(); got != base64.StdEncoding.EncodeToString([]byte("clip-bytes")) {
		t.Fatalf("data = %q", got)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.videoMetadata.fps").Int(); got != 2 {
		t.Fatalf("metadata lost: %s", out)
	}
}

func TestResolveRemoteVideoURLsRejectsForeignGatewayFile(t *testing.T) {
	store, err := upload.Open(t.TempDir(), config.FileStoreConfig{TTL: "1h", MaxUploadMB: 1, MaxTotalGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	prev := upload.Current()
	upload.SetCurrent(store)
	t.Cleanup(func() { upload.SetCurrent(prev) })

	meta, err := store.Put("alice", strings.NewReader("secret"), "video/mp4", "secret.mp4")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"gwfile://` + meta.ID + `"}}]}]}`)
	_, release, err := ResolveRemoteVideoURLs(context.Background(), &config.Config{}, payload, map[string]any{
		coreexecutor.CallerScopeMetadataKey: "bob",
	})
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("expected foreign file handle to be rejected")
	}
}
