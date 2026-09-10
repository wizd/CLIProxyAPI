package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/upload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

type fileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

func fileObjectFromMeta(meta upload.Meta) fileObject {
	return fileObject{
		ID:        meta.ID,
		Object:    "file",
		Bytes:     meta.Bytes,
		CreatedAt: meta.CreatedAt,
		ExpiresAt: meta.ExpiresAt,
		Filename:  meta.Filename,
		Purpose:   "user_data",
	}
}

func requestCallerScope(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return coresession.CallerScope(fmt.Sprint(value))
}

func resolveFileStoreDir(cfg *config.Config, configFilePath string) string {
	if cfg != nil {
		if dir := strings.TrimSpace(cfg.FileStore.Dir); dir != "" {
			return dir
		}
	}
	if info, err := os.Stat("/CLIProxyAPI/uploads"); err == nil && info.IsDir() {
		return "/CLIProxyAPI/uploads"
	}
	if configFilePath != "" {
		return filepath.Join(filepath.Dir(configFilePath), "uploads")
	}
	if cfg != nil {
		if resolved, err := util.ResolveAuthDir(cfg.AuthDir); err == nil && resolved != "" {
			return filepath.Join(resolved, "uploads")
		}
	}
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "uploads")
	}
	return "uploads"
}

func (s *Server) ensureFileStore(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	dir := resolveFileStoreDir(cfg, s.configFilePath)
	if s.fileStore != nil && s.fileStore.Dir() == dir {
		s.fileStore.ApplyConfig(cfg.FileStore)
		upload.SetCurrent(s.fileStore)
		return
	}
	if s.fileStore != nil {
		s.fileStore.Close()
		s.fileStore = nil
	}
	store, err := upload.Open(dir, cfg.FileStore)
	if err != nil {
		log.WithError(err).Errorf("failed to open file store at %s", dir)
		upload.SetCurrent(nil)
		return
	}
	s.fileStore = store
	upload.SetCurrent(store)
	log.Infof("file store ready at %s", dir)
}

func (s *Server) uploadFile(c *gin.Context) {
	if s == nil || s.fileStore == nil {
		writeFileError(c, http.StatusServiceUnavailable, "file store unavailable", "server_error")
		return
	}
	reader, err := c.Request.MultipartReader()
	if err != nil {
		writeFileError(c, http.StatusBadRequest, "multipart form with a file field is required", "invalid_request_error")
		return
	}

	for {
		part, errNext := reader.NextPart()
		if errors.Is(errNext, io.EOF) {
			break
		}
		if errNext != nil {
			writeFileError(c, http.StatusBadRequest, "failed to read multipart upload", "invalid_request_error")
			return
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
			_ = part.Close()
			continue
		}
		filename := part.FileName()
		mimeType := normalizeUploadMIME(part.Header.Get("Content-Type"), filename)
		if !strings.HasPrefix(mimeType, "video/") {
			_ = part.Close()
			writeFileError(c, http.StatusBadRequest, "only video uploads are accepted", "invalid_request_error")
			return
		}
		meta, errPut := s.fileStore.Put(requestCallerScope(c), part, mimeType, filename)
		_ = part.Close()
		if errPut != nil {
			writeUploadError(c, errPut)
			return
		}
		c.JSON(http.StatusOK, fileObjectFromMeta(meta))
		return
	}
	writeFileError(c, http.StatusBadRequest, "file is required", "invalid_request_error")
}

func (s *Server) getFile(c *gin.Context) {
	if s == nil || s.fileStore == nil {
		writeFileError(c, http.StatusServiceUnavailable, "file store unavailable", "server_error")
		return
	}
	meta, err := s.fileStore.Stat(requestCallerScope(c), c.Param("file_id"))
	if err != nil {
		writeUploadError(c, err)
		return
	}
	c.JSON(http.StatusOK, fileObjectFromMeta(meta))
}

func (s *Server) deleteFile(c *gin.Context) {
	if s == nil || s.fileStore == nil {
		writeFileError(c, http.StatusServiceUnavailable, "file store unavailable", "server_error")
		return
	}
	fileID := c.Param("file_id")
	if err := s.fileStore.Delete(requestCallerScope(c), fileID); err != nil {
		writeUploadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      fileID,
		"object":  "file",
		"deleted": true,
	})
}

func writeUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, upload.ErrNotFound), errors.Is(err, upload.ErrInvalidID):
		writeFileError(c, http.StatusNotFound, "file not found", "not_found")
	case errors.Is(err, upload.ErrTooLarge):
		writeFileError(c, http.StatusRequestEntityTooLarge, "file exceeds upload size limit", "invalid_request_error")
	case errors.Is(err, upload.ErrUnavailable):
		writeFileError(c, http.StatusServiceUnavailable, "file store unavailable", "server_error")
	default:
		writeFileError(c, http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
}

func writeFileError(c *gin.Context, status int, message, errType string) {
	c.JSON(status, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func normalizeUploadMIME(raw, filename string) string {
	raw = strings.TrimSpace(strings.Split(raw, ";")[0])
	if strings.HasPrefix(raw, "video/") {
		return raw
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext != "" {
		if mapped := misc.MimeTypes[ext]; strings.HasPrefix(mapped, "video/") {
			return mapped
		}
		if mapped := mime.TypeByExtension("." + ext); strings.HasPrefix(mapped, "video/") {
			return strings.TrimSpace(strings.Split(mapped, ";")[0])
		}
	}
	return raw
}

func isFileUploadPath(path string) bool {
	return path == "/v1/files"
}
