package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// makeEncodedEnvelope 用插件的 codec 造一个合法的加密 envelope
// （sha512(sha512(random32)+salt) 派生 key/IV，PKCS7 填充，明文前 64 字节为
// 自身 sha512）——自洽向量：同一 codec 的 Go 解密必须还原。
func makeEncodedEnvelope(t *testing.T, body any) string {
	t.Helper()
	salt := codecSalt["746305100000"]
	inner, _ := json.Marshal(body)
	sum := sha512.Sum512(inner)
	plain := append(sum[:], inner...)
	pad := 16 - len(plain)%16
	for range pad {
		plain = append(plain, byte(pad))
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	innerSum := sha512.Sum512(random)
	outer := sha512.Sum512(append(innerSum[:], salt...))
	block, err := aes.NewCipher(outer[:16])
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, outer[16:32]).CryptBlocks(out, plain)
	buf := append(append([]byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00}, random...), out...) // 盐头 "746305100000"
	return base64.StdEncoding.EncodeToString(buf)
}

// clientDecode 还原明文 envelope（sha512 校验通过）。
func TestClientDecodeRoundtrip(t *testing.T) {
	encoded := makeEncodedEnvelope(t, map[string]any{"userId": "7615755", "name": "tester"})
	body, err := clientDecode(encoded)
	if err != nil {
		t.Fatalf("clientDecode: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("解码结果不是 JSON: %v", err)
	}
	if plainString(claims["userId"]) != "7615755" {
		t.Fatalf("userId = %v", claims["userId"])
	}
}

// 错误 salt 头与损坏密文必须拒绝。
func TestClientDecodeRejects(t *testing.T) {
	encoded := makeEncodedEnvelope(t, map[string]any{"userId": "1"})
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	badSalt := append([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}, raw[6:]...)
	if _, err := clientDecode(base64.StdEncoding.EncodeToString(badSalt)); err == nil {
		t.Fatal("未知 salt 应拒绝")
	}
	corrupted := append([]byte{}, raw...)
	corrupted[len(corrupted)-3] ^= 0xFF
	if _, err := clientDecode(base64.StdEncoding.EncodeToString(corrupted)); err == nil {
		t.Fatal("密文损坏应拒绝")
	}
	if _, err := clientDecode("not-base64!!"); err == nil {
		t.Fatal("非 base64 应拒绝")
	}
}

// profile 枚举：storage.json 的 envelope userId 与签到账号匹配才返回设备 id；
// 多 profile（IDE + SOLO）各自绑定账号——0.2.22 的核心场景。
func TestDetectCheckinDeviceForUIDMatchesProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("TW2A_DISABLE_CHECKIN_DETECT", "")

	writeProfile := func(profile, uid, device string) {
		t.Helper()
		encoded := makeEncodedEnvelope(t, map[string]any{"userId": uid})
		doc := map[string]any{
			"iCubeAuthInfo://icube-dc:" + device: "1",
			"iCubeAuthInfo://icube.cloudide":     encoded,
		}
		p := filepath.Join(dir, profile, "User", "globalStorage", "storage.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(jsonDump(doc)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile("TRAE SOLO CN", "uid-solo", "1111111111111111")
	writeProfile("Trae CN", "uid-ide", "2222222222222222")

	id, _, _, ok := DetectCheckinDeviceForUID("uid-solo")
	if !ok || id != "1111111111111111" {
		t.Fatalf("uid-solo 应匹配 SOLO profile 的设备, got %q ok=%v", id, ok)
	}
	id, _, _, ok = DetectCheckinDeviceForUID("uid-ide")
	if !ok || id != "2222222222222222" {
		t.Fatalf("uid-ide 应匹配 IDE profile 的设备, got %q ok=%v", id, ok)
	}
	if _, _, _, ok := DetectCheckinDeviceForUID("uid-none"); ok {
		t.Fatal("无匹配 profile 应 ok=false")
	}
}

// 明文 envelope（未加密的 storage.json）同样可读。
func TestDetectCheckinDevicePlainEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("TW2A_DISABLE_CHECKIN_DETECT", "")
	p := filepath.Join(dir, "TRAE SOLO CN", "User", "globalStorage", "storage.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"iCubeAuthInfo://icube-dc:3333333333333333":"1","iCubeAuthInfo://icube.cloudide":"{\"userId\":\"uid-x\"}"}`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	id, _, _, ok := DetectCheckinDeviceForUID("uid-x")
	if !ok || id != "3333333333333333" {
		t.Fatalf("明文 envelope 应可读, got %q ok=%v", id, ok)
	}
}

// UgDevice：已具 15/16 位客户端形态的设备 id 原样返回；否则由登录设备
// 与 uid 稳定派生 16 位（1 开头）；两端与插件算法逐字节一致。
func TestUgDevice(t *testing.T) {
	if got := UgDevice("123456789012345", "u1"); got != "123456789012345" {
		t.Fatalf("15 位设备 id 应原样返回, got %q", got)
	}
	if got := UgDevice("1234567890123456", ""); got != "1234567890123456" {
		t.Fatalf("16 位设备 id 应原样返回, got %q", got)
	}
	if got := UgDevice("", ""); got != "" {
		t.Fatalf("登录设备与 uid 都为空应返回空串, got %q", got)
	}
	a := UgDevice("19digit-device-id-x", "7615755")
	if len(a) != 16 {
		t.Fatalf("派生 id 应为 16 位, got %q", a)
	}
	for range 20 {
		if again := UgDevice("19digit-device-id-x", "7615755"); again != a {
			t.Fatalf("派生应稳定: %q vs %q", a, again)
		}
	}
	// 与插件 Node 端逐字节一致的已知向量（ugDevice 源码直算）。
	for _, c := range [][2]string{{"dev", "u"}, {"19digit-device-id-x", "7615755"}} {
		if got := UgDevice(c[0], c[1]); got != a && len(got) != 16 {
			t.Fatalf("派生 id 应为 16 位, got %q", got)
		}
	}
	if got, want := UgDevice("dev", "u"), "5911771692791879"; got != want {
		t.Fatalf("与插件向量不符: got %q want %q", got, want)
	}
	if got, want := UgDevice("19digit-device-id-x", "7615755"), "4040202801445976"; got != want {
		t.Fatalf("与插件向量不符: got %q want %q", got, want)
	}
	// 不同账号派生结果不同（绝不共用设备）。
	if UgDevice("dev", "u2") == UgDevice("dev", "u") {
		t.Fatal("不同 uid 的派生 id 不应相同")
	}
}

// jsonDump 小助手。
func jsonDump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
