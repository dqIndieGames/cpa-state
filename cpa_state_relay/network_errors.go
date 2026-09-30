package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Never serialize Error(): URL wrappers, DNS names and proxy responses may
// contain credentials or request data. Keep typed transport diagnostics only.
func networkErrorDetails(err error) map[string]any {
	d := map[string]any{"kind": "transport"}
	var ne net.Error
	if errors.As(err, &ne) {
		d["timeout"] = ne.Timeout()
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "read", "write", "connect", "proxyconnect":
			d["operation"] = op.Op
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		d["errno"] = uint64(errno)
	}
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		d["kind"] = "deadline"
	case errors.Is(err, context.Canceled):
		d["kind"] = "canceled"
	case errors.Is(err, io.ErrUnexpectedEOF):
		d["kind"] = "unexpected_eof"
	case errors.Is(err, io.EOF):
		d["kind"] = "eof"
	case errors.As(err, &dns):
		d["kind"] = "dns"
	case errors.As(err, &cert), errors.As(err, &unknownCA):
		d["kind"] = "tls_certificate"
	case errors.As(err, &record):
		d["kind"] = "tls_record"
	}
	return d
}

var networkLogMu sync.Mutex

func (r *relay) logUpstreamError(req *http.Request, cause error, elapsed time.Duration) {
	id, _ := sessionIdentity(req, requestMetadata{})
	d := networkErrorDetails(cause)
	d["time"] = time.Now().UTC()
	d["elapsed_ms"] = elapsed.Milliseconds()
	d["session_hash"] = fmt.Sprintf("%x", sha256.Sum256([]byte(accountIdentity(req)+"\x00"+id)))
	d["route"] = "system"
	d["bps"] = bpsRequest(req)
	if err := r.appendNetworkError(d); err != nil {
		r.report(fmt.Errorf("无法保存网络错误诊断日志"))
	}
}

func (r *relay) appendNetworkError(entry map[string]any) error {
	networkLogMu.Lock()
	defer networkLogMu.Unlock()
	dir := filepath.Dir(r.settingsPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "network-errors.jsonl")
	if stat, err := os.Stat(path); err == nil && stat.Size() >= 1<<20 {
		if err := os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(entry)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
