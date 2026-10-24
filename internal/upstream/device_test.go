package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"traework2api/internal/auth"
)

// NewDeviceKey 产出可解析的 PKCS8 私钥 / SPKI 公钥，且公私钥匹配。
func TestNewDeviceKeyRoundtrip(t *testing.T) {
	pubPEM, privPEM, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := parseDevicePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("解析私钥: %v", err)
	}
	digest := sha256.Sum256([]byte("probe"))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatal("公钥 PEM 缺失")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("解析公钥: %v", err)
	}
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), digest[:], sig) {
		t.Fatal("公钥应能验证私钥签名")
	}
}

// DeviceProof 的签名可用公钥对同一段规范化文本验证（多行形状是契约）。
func TestDeviceProofVerifies(t *testing.T) {
	_, privPEM, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := parseDevicePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	const (
		path     = "/trae/api/v3/oauth/ExchangeToken"
		clientID = "en1oxy7wnw8j9n"
		refresh  = "rt-1"
		at       = int64(1792848000)
		nonce    = "00112233445566778899aabbccddeeff"
	)
	sigB64, err := deviceProof(priv, path, clientID, refresh, at, nonce)
	if err != nil {
		t.Fatalf("deviceProof: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("签名不是合法 base64: %v", err)
	}
	text := "POST\n" + path + "\n" + clientID + "\n" + refresh + "\n" + strconv.FormatInt(at, 10) + "\n" + nonce
	digest := sha256.Sum256([]byte(text))
	pub := &priv.PublicKey
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("签名验证失败")
	}
	// 规范化文本任一行变化都应导致验证失败。
	badDigest := sha256.Sum256([]byte(text + "\n"))
	if ecdsa.VerifyASN1(pub, badDigest[:], sig) {
		t.Fatal("文本变化不应验证通过")
	}
}

// 设备绑定刷新：SOLO 身份被拒后落 IDE 身份成功；DeviceInfo/DeviceProof 形状
// 正确、设备密钥随账号持久化；绑定通道成功时不轮换 refresh_token。
func TestDeviceBoundExchangeFallsBackToIDEClient(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceExchangePath {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT jwt-live" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if clientID, _ := body["ClientID"].(string); clientID == deviceClients[1].ClientID {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"Token":         "at-new",
				"TokenExpireAt": 1792848000000,
			}})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_msg":"refresh token has been used"}`))
	}))
	defer srv.Close()

	a := &auth.Auth{
		UID: "u1", RefreshToken: "rt-old", AccessToken: "jwt-live",
		DeviceID: "dev-1", MachineID: "mach-1", ApiHost: srv.URL,
	}
	c := New()
	access, refresh, expires, err := c.deviceBoundExchange(context.Background(), a)
	if err != nil {
		t.Fatalf("deviceBoundExchange: %v", err)
	}
	if access != "at-new" || refresh != "rt-old" {
		t.Fatalf("access=%q refresh=%q", access, refresh)
	}
	if expires != 1792848000 {
		t.Fatalf("expires = %d, 期望毫秒归一化为秒 1792848000", expires)
	}
	if a.DevicePublicKey == "" || a.DevicePrivateKey == "" {
		t.Fatal("设备密钥应写回账号（下次刷新同一把签名）")
	}
	if len(bodies) != 2 {
		t.Fatalf("应依次尝试两条 client 身份, 实际 %d", len(bodies))
	}
	first := bodies[0]
	if first["ClientID"] != deviceClients[0].ClientID {
		t.Fatalf("首条 ClientID = %v", first["ClientID"])
	}
	info, _ := first["DeviceInfo"].(map[string]any)
	if info == nil || info["DevicePublicKey"] != a.DevicePublicKey {
		t.Fatalf("DeviceInfo.DevicePublicKey 应与持久化公钥一致: %v", info)
	}
	proof, _ := first["DeviceProof"].(map[string]any)
	nonce, _ := proof["Nonce"].(string)
	if len(nonce) != 32 {
		t.Fatalf("DeviceProof.Nonce = %v", proof["Nonce"])
	}
}

// 绑定通道全败（两条 client 身份都 400）→ 返回错误，由调用方回退通用通道。
func TestDeviceBoundExchangeFailsGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	a := &auth.Auth{UID: "u1", RefreshToken: "rt-old", AccessToken: "jwt-live", ApiHost: srv.URL}
	c := New()
	if _, _, _, err := c.deviceBoundExchange(context.Background(), a); err == nil {
		t.Fatal("全败应返回错误")
	}
	if a.DevicePublicKey == "" {
		t.Fatal("为本次尝试生成的设备密钥应写回账号（插件 keys go with either）")
	}
}
