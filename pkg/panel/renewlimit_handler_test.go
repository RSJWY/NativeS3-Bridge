package panel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// R7(AC6):端到端验证 /renew 的限频接线——限频器本身有单元测试,这里确认它真的挂在
// handler 上、超限时返回 429 并带 Retry-After、且拒绝发生在签发之前(不多插证书行)。
func TestRenewHandlerRateLimitsAfterQuotaExhausted(t *testing.T) {
	gdb := openTestDB(t)
	ca := newTestIntermediateCA(t)
	ts := NewTransportServer(TransportDeps{DB: gdb, CA: ca, Hub: NewHub()})

	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	clientCert, _ := renewTestCert(t, gdb, ca, node.ID)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
	}, key)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	body := mustJSON(t, renewRequest{CSRPEM: string(csrPEM)})

	srv := startTestServer(t, ts, ca)
	defer srv.Close()
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true,
	}}}

	renew := func() *http.Response {
		resp, err := httpClient.Post(srv.URL+"/renew", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("renew request: %v", err)
		}
		return resp
	}

	// 额度内的续期照常成功。
	for i := 0; i < maxRenewPerWindow; i++ {
		resp := renew()
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			t.Fatalf("renew #%d status = %d, want 200 (still within quota)", i+1, status)
		}
	}

	// 超出额度:429 + Retry-After。
	resp := renew()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("renew #%d status = %d, want 429", maxRenewPerWindow+1, resp.StatusCode)
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("rate-limited renew must carry a Retry-After header")
	}
	if seconds, err := strconv.Atoi(retryAfter); err != nil || seconds <= 0 {
		t.Fatalf("Retry-After = %q, want a positive integer number of seconds", retryAfter)
	}

	// 被拒的请求不得签发证书:1 张初始 + maxRenewPerWindow 张续期,不多不少。
	var certCount int64
	gdb.Model(&NodeCert{}).Where("node_id = ?", node.ID).Count(&certCount)
	if want := int64(maxRenewPerWindow + 1); certCount != want {
		t.Fatalf("cert rows = %d, want %d (the rejected renew must not sign anything)", certCount, want)
	}

	// 限频要留下审计痕迹,便于运维发现异常循环。
	var auditCount int64
	gdb.Model(&AuditLog{}).
		Where("action = ? AND target_node = ? AND result = ?", "node_cert_renew", node.ID, "rate_limited").
		Count(&auditCount)
	if auditCount != 1 {
		t.Fatalf("rate_limited audit entries = %d, want 1", auditCount)
	}
}
