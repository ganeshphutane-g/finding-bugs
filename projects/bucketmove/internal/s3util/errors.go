// Package s3util holds small helpers shared by the bucketmove packages.
package s3util

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/minio/minio-go/v7"
)

// Code returns the S3 error code ("NoSuchKey", "NotImplemented", ...), or "".
func Code(err error) string {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code
	}
	return ""
}

func status(err error) int {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode
	}
	return 0
}

// notConfigured lists the codes servers use for "this setting is not set".
var notConfigured = map[string]bool{
	"NoSuchLifecycleConfiguration":                   true,
	"NoSuchBucketPolicy":                             true,
	"NoSuchCORSConfiguration":                        true,
	"NoSuchTagSet":                                   true,
	"NoSuchTagSetError":                              true,
	"ServerSideEncryptionConfigurationNotFoundError": true,
	"ObjectLockConfigurationNotFoundError":           true,
	"NoSuchObjectLockConfiguration":                  true,
	"ReplicationConfigurationNotFoundError":          true,
	"NoSuchWebsiteConfiguration":                     true,
	"NoSuchObjectLegalHold":                          true,
	"NoSuchObjectLockRetention":                      true,
}

// NotConfigured reports whether err means the setting simply is not set.
func NotConfigured(err error) bool {
	return err != nil && notConfigured[Code(err)]
}

// NotSupported reports whether err means the server does not implement the
// call at all.
func NotSupported(err error) bool {
	if err == nil {
		return false
	}
	switch Code(err) {
	case "NotImplemented", "MethodNotAllowed", "UnsupportedOperation", "XNotImplemented":
		return true
	}
	if s := status(err); s == http.StatusNotImplemented {
		return true
	}
	// Only errors without an S3 code are judged by their text; a coded
	// error (InvalidRequest, AccessDenied, ...) is a real failure.
	if Code(err) != "" {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not implemented") || strings.Contains(msg, "unimplemented") || strings.Contains(msg, "not supported")
}

// Transient reports whether err is worth retrying: a server-side hiccup or a
// dropped connection.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	switch Code(err) {
	case "InternalError", "SlowDown", "ServiceUnavailable", "RequestTimeout", "Throttling", "RequestTimeTooSkewed":
		return true
	}
	if s := status(err); s >= 500 {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
