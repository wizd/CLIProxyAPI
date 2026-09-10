package helps

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errRemoteVideoBlocked = errors.New("remote video URL is not allowed")

// skipRemoteVideoSSRFForTest lets unit tests fetch from httptest (loopback).
var skipRemoteVideoSSRFForTest bool

// ResolveRemoteVideoURLs inlines non-YouTube http(s) fileData parts when fetching
// is enabled. YouTube URLs stay as fileData. Other remote URLs return an error
// when fetching is disabled so the client is not met with an opaque upstream 400.
func ResolveRemoteVideoURLs(ctx context.Context, cfg *config.Config, payload []byte) ([]byte, error) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, nil
	}
	contentsPath := "contents"
	if gjson.GetBytes(payload, "request.contents").Exists() {
		contentsPath = "request.contents"
	}
	contents := gjson.GetBytes(payload, contentsPath)
	if !contents.IsArray() {
		return payload, nil
	}

	videoCfg := config.VideoConfig{}
	if cfg != nil {
		videoCfg = cfg.Video
	}

	type replacement struct {
		path    string
		part    []byte
		fetch   bool
		uri     string
		mime    string
		snake   bool
		metaRaw string
	}
	var pending []replacement
	contents.ForEach(func(ci, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(pi, part gjson.Result) bool {
			uri, mime, ok := geminiFileDataRef(part)
			if !ok || !translatorcommon.IsRemoteHTTPURL(uri) || translatorcommon.IsYouTubeURL(uri) {
				return true
			}
			pending = append(pending, replacement{
				path:    fmt.Sprintf("%s.%d.parts.%d", contentsPath, ci.Int(), pi.Int()),
				fetch:   videoCfg.FetchRemoteURLs,
				uri:     uri,
				mime:    mime,
				snake:   part.Get("file_data").Exists(),
				metaRaw: part.Get("videoMetadata").Raw,
			})
			return true
		})
		return true
	})
	if len(pending) == 0 {
		return payload, nil
	}

	for _, item := range pending {
		if !item.fetch {
			return payload, fmt.Errorf("remote video URL is not fetched by default; use a YouTube URL, inline base64, or enable video.fetch-remote-urls: %s", item.uri)
		}
		data, mime, errFetch := fetchRemoteVideo(ctx, videoCfg, item.uri, item.mime)
		if errFetch != nil {
			return payload, errFetch
		}
		kind := translatorcommon.VideoPartGeminiCamel
		if item.snake {
			kind = translatorcommon.VideoPartResponses
		}
		part := translatorcommon.GeminiInlineDataPart(mime, data, kind)
		if item.metaRaw != "" {
			part = translatorcommon.AttachVideoMetadata(part, []byte(item.metaRaw))
		}
		var errSet error
		payload, errSet = sjson.SetRawBytes(payload, item.path, part)
		if errSet != nil {
			return payload, errSet
		}
	}
	return payload, nil
}

func geminiFileDataRef(part gjson.Result) (uri, mime string, ok bool) {
	node := part.Get("fileData")
	if !node.Exists() {
		node = part.Get("file_data")
	}
	if !node.Exists() {
		return "", "", false
	}
	uri = strings.TrimSpace(node.Get("fileUri").String())
	if uri == "" {
		uri = strings.TrimSpace(node.Get("file_uri").String())
	}
	mime = strings.TrimSpace(node.Get("mimeType").String())
	if mime == "" {
		mime = strings.TrimSpace(node.Get("mime_type").String())
	}
	return uri, mime, uri != ""
}

func fetchRemoteVideo(ctx context.Context, cfg config.VideoConfig, rawURL, fallbackMIME string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid video URL: %w", err)
	}
	if err = validateRemoteVideoURL(parsed, cfg.FetchAllowedHosts); err != nil {
		return "", "", err
	}

	timeout := 30 * time.Second
	if cfg.FetchTimeout != "" {
		if parsedTimeout, errTimeout := time.ParseDuration(cfg.FetchTimeout); errTimeout == nil && parsedTimeout > 0 {
			timeout = parsedTimeout
		}
	}
	limit := cfg.FetchLimitBytes()
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "CLIProxyAPI-video-fetch/1.0")

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
				host, port, errSplit := net.SplitHostPort(address)
				if errSplit != nil {
					host = address
				}
				ips, errLookup := net.DefaultResolver.LookupIPAddr(dialCtx, host)
				if errLookup != nil {
					return nil, errLookup
				}
				var lastErr error
				for _, ip := range ips {
					if !skipRemoteVideoSSRFForTest && !isPublicIP(ip.IP) {
						lastErr = errRemoteVideoBlocked
						continue
					}
					dialer := &net.Dialer{Timeout: 10 * time.Second}
					target := net.JoinHostPort(ip.IP.String(), port)
					conn, errDial := dialer.DialContext(dialCtx, network, target)
					if errDial == nil {
						return conn, nil
					}
					lastErr = errDial
				}
				if lastErr == nil {
					lastErr = errRemoteVideoBlocked
				}
				return nil, lastErr
			},
		},
		CheckRedirect: func(redirect *http.Request, _ []*http.Request) error {
			return validateRemoteVideoURL(redirect.URL, cfg.FetchAllowedHosts)
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch video URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("fetch video URL: unexpected status %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return "", "", fmt.Errorf("remote video exceeds %d MB limit", limit>>20)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", "", fmt.Errorf("read video URL: %w", err)
	}
	if int64(len(body)) > limit {
		return "", "", fmt.Errorf("remote video exceeds %d MB limit", limit>>20)
	}
	if len(body) == 0 {
		return "", "", fmt.Errorf("remote video is empty")
	}

	mime := fallbackMIME
	if ct := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); strings.HasPrefix(ct, "video/") {
		mime = ct
	}
	if mime == "" {
		mime = "video/mp4"
	}
	return base64.StdEncoding.EncodeToString(body), mime, nil
}

func validateRemoteVideoURL(parsed *url.URL, allowedHosts []string) error {
	if parsed == nil {
		return errRemoteVideoBlocked
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return errRemoteVideoBlocked
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errRemoteVideoBlocked
	}
	if translatorcommon.IsYouTubeURL(parsed.String()) {
		return nil
	}
	if !hostAllowed(host, allowedHosts) {
		return fmt.Errorf("%w: host %s is not in video.fetch-allowed-hosts", errRemoteVideoBlocked, host)
	}
	if ip := net.ParseIP(host); ip != nil && !skipRemoteVideoSSRFForTest && !isPublicIP(ip) {
		return errRemoteVideoBlocked
	}
	return nil
}

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return true
		}
	}
	return false
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return false
		}
	}
	return true
}
