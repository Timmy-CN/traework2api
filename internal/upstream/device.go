package upstream

// 设备绑定刷新（插件 boundExchange/renewal 口径）：
//
//	POST {authHost}/trae/api/v3/oauth/ExchangeToken
//	body: {ClientID, ClientSecret:"", RefreshToken, DeviceInfo, DeviceProof, IDEVersion}
//
// DeviceInfo 携带设备三件套与 SPKI 公钥；DeviceProof 是设备私钥（ECDSA
// P-256，登录后生成并持久化）对 `POST\n<path>\n{ClientID}\n{RefreshToken}\n
// {Timestamp}\n{Nonce}` 的 SHA256/ASN.1 DER 签名。该通道不消耗 refresh_token、
// 不踢该账号的其他客户端；两条 client 身份（SOLO_PC → IDE_PC）依次尝试，
// 全部失败由调用方回退通用轮换通道（会轮换并作废旧 refresh_token）。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"traework2api/internal/auth"
)

// 与官方客户端一致的网络/设备形态常量（插件 deviceInfo 同值）。
const (
	deviceExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	deviceBrand        = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	deviceCPU          = "AMD"
	deviceOSInfo       = "Windows"
	deviceOSVersion    = "10.0.22631"
	deviceType         = "PC"
	deviceClientVerA   = "0.1.64"
	deviceClientVerB   = "3.3.99"
)

// deviceClient 是一次设备绑定刷新冒名的 client 身份：先 SOLO 后 IDE。
type deviceClient struct {
	ClientID string
	Platform string
	Version  string
}

var deviceClients = []deviceClient{
	{ClientID: "en1oxy7wnw8j9n", Platform: "SOLO_PC", Version: deviceClientVerA},
	{ClientID: "ono9krqynydwx5", Platform: "IDE_PC", Version: deviceClientVerB},
}

// NewDeviceKey 生成 ECDSA P-256 设备密钥对，返回 (SPKI PEM, PKCS8 PEM)。
// 插件 newDeviceKey 同形态（devicePublicKey/devicePrivateKey）。
func NewDeviceKey() (pubPEM, privPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return pubPEM, privPEM, nil
}

// parseDevicePrivateKey 解析 PKCS8 PEM 私钥。
func parseDevicePrivateKey(privPEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return nil, fmt.Errorf("device private key: no PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// deviceProof 生成 DeviceProof：设备私钥对
// "POST\n<path>\n{ClientID}\n{RefreshToken}\n{Timestamp}\n{Nonce}" 的
// ECDSA-SHA256（ASN.1 DER，base64）签名，附 Unix 秒时间戳与 16 字节 hex nonce。
func deviceProof(priv *ecdsa.PrivateKey, path, clientID, refreshToken string, at int64, nonce string) (string, error) {
	text := fmt.Sprintf("POST\n%s\n%s\n%s\n%d\n%s", path, clientID, refreshToken, at, nonce)
	digest := sha256.Sum256([]byte(text))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// randomRequestID 生成 x-request-id 用的 v4 UUID。
func randomRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// randomNonceHex 生成 n 字节的 hex 随机串（DeviceProof 的 Nonce）。
func randomNonceHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// deviceBoundExchange 用设备绑定通道刷新 access token。
//
// 返回 (新 accessToken, 新 refreshToken（绑定通道不轮换，未下发时原值）,
// 过期 Unix 秒, err)。a 的设备密钥缺失时懒生成并就地写回——调用方 SaveAtomic
// 时持久化，下次刷新用同一把密钥签名（插件：keys go with either）。两条
// client 身份依次尝试；全部失败返回最后一次错误，由调用方回退通用通道。
func (c *Client) deviceBoundExchange(ctx context.Context, a *auth.Auth) (access, refresh string, expiresAt int64, err error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return "", "", 0, fmt.Errorf("no refreshToken")
	}
	pub, priv := a.DeviceKeys()
	if pub == "" || priv == "" {
		if pub, priv, err = NewDeviceKey(); err != nil {
			return "", "", 0, err
		}
		a.DevicePublicKey = pub
		a.DevicePrivateKey = priv
	}
	privKey, perr := parseDevicePrivateKey(priv)
	if perr != nil {
		return "", "", 0, perr
	}
	host := a.ApiHost
	if host == "" {
		host = c.ugBase()
	}
	path := deviceExchangePath
	refresh = strings.TrimSpace(a.RefreshToken)

	var lastErr error
	for _, as := range deviceClients {
		at := time.Now().Unix()
		nonce := randomNonceHex(16)
		signature, perr := deviceProof(privKey, path, as.ClientID, refresh, at, nonce)
		if perr != nil {
			lastErr = perr
			continue
		}
		body := map[string]any{
			"ClientID":     as.ClientID,
			"ClientSecret": "",
			"RefreshToken": refresh,
			"DeviceInfo": map[string]any{
				"DeviceID":        a.DeviceID,
				"MachineID":       a.MachineID,
				"PlatformCode":    as.Platform,
				"DeviceType":      deviceType,
				"DeviceName":      "",
				"DeviceModel":     "",
				"ClientVersion":   as.Version,
				"DevicePublicKey": pub,
				"DeviceBrand":     deviceBrand,
				"DeviceCPU":       deviceCPU,
				"OSInfo":          deviceOSInfo,
				"OSVersion":       deviceOSVersion,
			},
			"DeviceProof": map[string]any{
				"Signature": signature,
				"Timestamp": at,
				"Nonce":     nonce,
			},
			"IDEVersion": as.Version,
		}
		raw, _ := json.Marshal(body)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, host+path, bytes.NewReader(raw))
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		// 调用方 refreshLocked 已持有 a.mu 写锁：直接读字段，不得用 a.JWT()
		//（会 RLock 自锁）。旧 token 仍有效时带上（插件同口径），已过期则匿名。
		if jwt := a.AccessToken; jwt != "" {
			req.Header.Set("Authorization", "Cloud-IDE-JWT "+jwt)
		}
		resp, doErr := c.HTTP.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("device exchange: HTTP %d: %s", resp.StatusCode, truncate(string(rawResp), 200))
			continue
		}
		var parsed struct {
			Result struct {
				Token               string `json:"Token"`
				TokenExpireAt       int64  `json:"TokenExpireAt"`
				TokenExpireDuration int64  `json:"TokenExpireDuration"`
				RefreshToken        string `json:"RefreshToken"`
			} `json:"Result"`
			ResultLower struct {
				Token               string `json:"token"`
				TokenExpireAt       int64  `json:"tokenExpireAt"`
				TokenExpireDuration int64  `json:"tokenExpireDuration"`
				RefreshToken        string `json:"refreshToken"`
			} `json:"result"`
		}
		if json.Unmarshal(rawResp, &parsed) != nil {
			lastErr = fmt.Errorf("parse device exchange: %s", truncate(string(rawResp), 200))
			continue
		}
		token := parsed.Result.Token
		newRefresh := parsed.Result.RefreshToken
		expireAt := parsed.Result.TokenExpireAt
		duration := parsed.Result.TokenExpireDuration
		if token == "" {
			token = parsed.ResultLower.Token
			if newRefresh == "" {
				newRefresh = parsed.ResultLower.RefreshToken
			}
			if expireAt == 0 {
				expireAt = parsed.ResultLower.TokenExpireAt
			}
			if duration == 0 {
				duration = parsed.ResultLower.TokenExpireDuration
			}
		}
		if token == "" {
			lastErr = fmt.Errorf("device exchange: no token: %s", truncate(string(rawResp), 200))
			continue
		}
		if newRefresh == "" {
			newRefresh = refresh // 绑定通道不轮换：沿用原值
		}
		// TokenExpireAt 优先（插件 whenOf 口径），缺失才按 duration 从现在起算。
		expires := normalizeExpiresAt(expireAt)
		if expires == 0 && duration > 0 {
			expires = time.Now().Add(time.Duration(duration) * time.Second).Unix()
		}
		return token, newRefresh, expires, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("device exchange: no client accepted")
	}
	return "", "", 0, lastErr
}
