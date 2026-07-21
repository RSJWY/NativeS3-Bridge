package nodeagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// registerRequest mirrors the panel's transport.registerRequest wire shape.
type registerRequest struct {
	NodeID int64  `json:"node_id"`
	Token  string `json:"token"`
	CSRPEM string `json:"csr_pem"`
}

// registerResponse mirrors the panel's transport.registerResponse wire shape.
type registerResponse struct {
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
	NotAfter  string `json:"not_after"`
}

// Identity holds the node's on-disk mTLS identity file paths. The private key
// is generated locally and never leaves the node (design §3.2).
type Identity struct {
	KeyFile  string // node private key (PEM, PKCS#8)
	CertFile string // issued client certificate (PEM)
	CAFile   string // panel intermediate CA certificate (PEM) for server verification
}

// HasCertificate reports whether a previously-issued client certificate already
// exists on disk, so the node can skip re-registration and dial with mTLS.
func (id Identity) HasCertificate() bool {
	if _, err := os.Stat(id.CertFile); err != nil {
		return false
	}
	if _, err := os.Stat(id.KeyFile); err != nil {
		return false
	}
	return true
}

// ensureKey loads the node private key from KeyFile, generating and persisting a
// new P-256 key on first boot. The key is written with 0600 permissions.
func (id Identity) ensureKey() (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(id.KeyFile); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("node key %q is not valid PEM", id.KeyFile)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse node key: %w", err)
		}
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("node key is not an ECDSA key")
		}
		return ecKey, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate node key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal node key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.MkdirAll(filepath.Dir(id.KeyFile), 0o700); err != nil {
		return nil, fmt.Errorf("create key dir: %w", err)
	}
	if err := os.WriteFile(id.KeyFile, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write node key: %w", err)
	}
	return key, nil
}

// buildCSR generates a PEM-encoded certificate signing request for nodeID using
// the node's private key. The subject CN matches the panel's issuance convention
// so operators can correlate the CSR to the logical node.
func buildCSR(key *ecdsa.PrivateKey, nodeID int64) ([]byte, error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// RegisterParams configures a one-shot registration attempt.
type RegisterParams struct {
	// RegisterURL is the panel's server-TLS registration endpoint, e.g.
	// https://panel:PORT/register. The panel identity is verified via server TLS
	// using CAFile (the node must already trust the panel's server CA / cert).
	RegisterURL string
	NodeID      int64
	Token       string
	Timeout     time.Duration
	// HTTPClient overrides the client used to reach the panel (for server-TLS
	// pinning or tests). When nil a default client with Timeout is used.
	HTTPClient *http.Client
}

// RegistrationError classifies a failed registration attempt for retry policy.
// Error messages never include the registration token or CSR.
type RegistrationError struct {
	StatusCode int
	Retryable  bool
	Err        error
}

func (e *RegistrationError) Error() string { return e.Err.Error() }
func (e *RegistrationError) Unwrap() error { return e.Err }

// RegisterRetryOptions controls exponential backoff between transient failures.
type RegisterRetryOptions struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Register performs first-boot registration: it ensures a local private key,
// builds a CSR, submits {node_id, token, csr} to the panel over server TLS, and
// persists the issued client certificate and the panel CA to disk. The private
// key never leaves the node. It is safe to skip when Identity.HasCertificate().
func Register(id Identity, params RegisterParams) error {
	return RegisterContext(context.Background(), id, params)
}

// RegisterContext performs one registration attempt and honors cancellation.
func RegisterContext(ctx context.Context, id Identity, params RegisterParams) error {
	if params.RegisterURL == "" || params.NodeID <= 0 || params.Token == "" {
		return permanentRegistrationError(0, fmt.Errorf("register url, node id and token are required"))
	}
	key, err := id.ensureKey()
	if err != nil {
		return permanentRegistrationError(0, err)
	}
	csrPEM, err := buildCSR(key, params.NodeID)
	if err != nil {
		return err
	}

	body, err := json.Marshal(registerRequest{
		NodeID: params.NodeID,
		Token:  params.Token,
		CSRPEM: string(csrPEM),
	})
	if err != nil {
		return fmt.Errorf("marshal register request: %w", err)
	}

	client := params.HTTPClient
	if client == nil {
		timeout := params.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		caPEM, err := os.ReadFile(id.CAFile)
		if err != nil {
			return permanentRegistrationError(0, fmt.Errorf("read panel CA: %w", err))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return permanentRegistrationError(0, fmt.Errorf("panel CA file contains no certificates"))
		}
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			}},
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, params.RegisterURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return transientRegistrationError(fmt.Errorf("submit registration: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		message := fmt.Sprintf("registration rejected with status %d", resp.StatusCode)
		if errBody.Error != "" {
			message = fmt.Sprintf("registration rejected (%d): %s", resp.StatusCode, errBody.Error)
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return &RegistrationError{StatusCode: resp.StatusCode, Retryable: retryable, Err: fmt.Errorf("%s", message)}
	}

	var issued registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		return fmt.Errorf("decode registration response: %w", err)
	}
	if issued.CertPEM == "" {
		return fmt.Errorf("panel returned an empty certificate")
	}

	if err := persistPEM(id.CertFile, []byte(issued.CertPEM), 0o644); err != nil {
		return fmt.Errorf("write client cert: %w", err)
	}
	if issued.CACertPEM != "" {
		if err := persistPEM(id.CAFile, []byte(issued.CACertPEM), 0o644); err != nil {
			return fmt.Errorf("write panel CA: %w", err)
		}
	}
	return nil
}

// RegisterWithRetry retries transient network/TLS, 429, and 5xx failures until
// registration succeeds, a permanent rejection occurs, or ctx is cancelled.
func RegisterWithRetry(ctx context.Context, id Identity, params RegisterParams, opts RegisterRetryOptions) error {
	delay := opts.InitialBackoff
	if delay <= 0 {
		delay = time.Second
	}
	maxDelay := opts.MaxBackoff
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	for {
		err := RegisterContext(ctx, id, params)
		if err == nil {
			return nil
		}
		var registrationErr *RegistrationError
		if !errors.As(err, &registrationErr) || !registrationErr.Retryable {
			return err
		}
		jittered := time.Duration(float64(delay) * (0.8 + mathrand.Float64()*0.4))
		timer := time.NewTimer(jittered)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func permanentRegistrationError(status int, err error) error {
	return &RegistrationError{StatusCode: status, Retryable: false, Err: err}
}

func transientRegistrationError(err error) error {
	return &RegistrationError{Retryable: true, Err: err}
}

func persistPEM(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}
