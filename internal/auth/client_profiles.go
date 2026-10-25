package auth

// client_profiles.go 本机 Trae CN / TRAE SOLO CN 客户端 profile 的签到设备
// 读取（插件 0.2.22 clientDevice/clientDecode 口径）。
//
// 一台机器可以同时装有多个客户端 profile（IDE 的 Trae CN 与 SOLO 的
// TRAE SOLO CN 各写一份，--user-data-dir 自定义的也各写一份），每个 profile
// 的 storage.json 记着「该 profile 登录的是哪个账号、用哪个设备」：
//   - `iCubeAuthInfo://icube-dc:<id>`：设备 id（8~20 位数字），一个 profile
//     有且仅有一个才有效；
//   - `iCubeAuthInfo://icube.cloudide`：登录 envelope（明文 JSON，或客户端
//     AES-128-CBC 编码，见 clientDecode），解出 userId。
// 所以枚举每个 profile、解出登录账号，与签到账号匹配的 profile 的设备 id
// 才是它的——这正是多账号各自用自己客户端设备签到的前提；一个 profile 读
// 不出或不是该账号的，跳过继续找，而不是以第一个命中的决定一切。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// clientProfiles 是 Trae CN 客户端 profile 的目录名：IDE 的与 SOLO 的
// （同 realm 的另一个客户端，各自登录自己的账号），加小写兼容形态。
var clientProfiles = []string{"Trae CN", "TRAE SOLO CN", "trae-cn"}

// dcKey/authKey 是 storage.json 里设备与登录 envelope 的键名前缀。
const (
	dcKey   = "iCubeAuthInfo://icube-dc:"
	authKey = "iCubeAuthInfo://icube.cloudide"
)

// clientProfileStorageFiles 返回本机客户端 profile 的 storage.json 路径，
// 按优先级（IDE 形态在前）；Windows 的两个 root（%APPDATA% 与
// ~/AppData/Roaming）常是同一目录，按系统书写形式归一比较，避免重复读。
func clientProfileStorageFiles() []string {
	var files []string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		for _, name := range clientProfiles {
			if name == "trae-cn" {
				continue // mac 上无小写形态
			}
			files = append(files, filepath.Join(home, "Library", "Application Support", name, "User", "globalStorage", "storage.json"))
		}
	case "windows":
		// 两个 root 常是同一目录：APPDATA 本身就是 Roaming，home 的
		// AppData\Roaming 是它的另一种拼法——归一比较去重。
		home, _ := os.UserHomeDir()
		var roots []string
		for _, r := range []string{os.Getenv("APPDATA"), filepath.Join(home, "AppData", "Roaming")} {
			if r == "" {
				continue
			}
			dup := false
			for _, was := range roots {
				if strings.EqualFold(filepath.Clean(was), filepath.Clean(r)) {
					dup = true
					break
				}
			}
			if !dup {
				roots = append(roots, r)
			}
		}
		for _, root := range roots {
			for _, name := range clientProfiles {
				files = append(files, filepath.Join(root, name, "User", "globalStorage", "storage.json"))
			}
		}
	case "linux":
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			base = filepath.Join(home, ".config")
		}
		for _, name := range clientProfiles {
			files = append(files, filepath.Join(base, name, "User", "globalStorage", "storage.json"))
		}
	}
	return files
}

// clientEnvelope 解 storage.json：返回该 profile 的设备 id 与登录账号
// userId。设备键有且仅有一个、id 为 8~20 位数字，envelope 解不出时 ok=false。
func clientEnvelope(doc map[string]any) (deviceID, userID string, ok bool) {
	ids := make([]string, 0, 2)
	for k := range doc {
		if strings.HasPrefix(k, dcKey) {
			ids = append(ids, strings.TrimPrefix(k, dcKey))
		}
	}
	if len(ids) != 1 {
		return "", "", false
	}
	id := strings.TrimSpace(ids[0])
	if len(id) < 8 || len(id) > 20 {
		return "", "", false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return "", "", false
		}
	}
	raw, ok := doc[authKey].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", "", false
	}
	var claims struct {
		UserId   any `json:"userId"`
		Username any `json:"username"`
	}
	if err := decodeEnvelope(raw, &claims); err != nil {
		return "", "", false
	}
	uid := strings.TrimSpace(plainString(claims.UserId))
	if uid == "" {
		return "", "", false
	}
	return id, uid, true
}

func plainString(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case float64:
		if s == float64(int64(s)) {
			return fmt.Sprintf("%d", int64(s))
		}
	}
	return ""
}

// decodeEnvelope 解登录 envelope：明文 JSON，或客户端 AES-128-CBC 编码
// （clientDecode）后是 JSON。
func decodeEnvelope(v string, out any) error {
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal([]byte(trimmed), out)
	}
	decoded, err := clientDecode(trimmed)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, out)
}

// codecSalt 是客户端 codec 的两份盐表（6 字节头 hex → 64 字节盐），照插件
// CODEC 抄录（Node xorOf 的两份 64 字节常量，init 时异或合成）。
var codecSalt = map[string][]byte{}

var codecSaltSources = map[string][2][]byte{
	"746305100000": {
		{82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251, 124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203, 84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78, 8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37},
		{31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95, 96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239, 160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97, 23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125},
	},
	"123920200203": {
		{191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176, 135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156, 51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6, 18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209},
		{246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145, 50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106, 175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154, 86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170},
	},
}

func init() {
	for key, pair := range codecSaltSources {
		salt := make([]byte, len(pair[0]))
		for i := range pair[0] {
			salt[i] = pair[0][i] ^ pair[1][i]
		}
		codecSalt[key] = salt
	}
}

// clientDecode 解开客户端的编码值：base64 → 前 6 字节是盐头（选 codecSalt），
// 随后 32 字节随机 + AES-128-CBC（key = sha512(sha512(random) + salt) 的前
// 16 字节，IV 为 17~32 字节）；明文前 64 字节是自身的 sha512 校验，其后是
// JSON envelope（PKCS7 填充在密文中，解密后由校验天然覆盖）。
func clientDecode(b64 string) ([]byte, error) {
	buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("client value not base64: %w", err)
	}
	if len(buf) <= 102 {
		return nil, fmt.Errorf("not the client's codec (too short)")
	}
	salt, ok := codecSalt[hex.EncodeToString(buf[:6])]
	if !ok {
		return nil, fmt.Errorf("not the client's codec (unknown salt)")
	}
	inner := sha512.Sum512(buf[6:38])
	outer := sha512.Sum512(append(inner[:], salt...))
	block, err := aes.NewCipher(outer[:16])
	if err != nil {
		return nil, err
	}
	ciphertext := buf[38:]
	if len(ciphertext)%16 != 0 {
		return nil, fmt.Errorf("client value not block-aligned")
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, outer[16:32]).CryptBlocks(plain, ciphertext)
	plain = pkcs7Unpad(plain, 16)
	if len(plain) < 65 {
		return nil, fmt.Errorf("client value too short after decrypt")
	}
	body := plain[64:]
	if !bytes.Equal(plain[:64], sha512Sum(body)) {
		return nil, fmt.Errorf("client value doesn't check (sha512)")
	}
	return body, nil
}

func sha512Sum(b []byte) []byte {
	sum := sha512.Sum512(b)
	return sum[:]
}

// pkcs7Unpad 去 PKCS7 填充；非法填充原样返回（外层 sha512 校验会兜底）。
func pkcs7Unpad(data []byte, blockSize int) []byte {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

// DetectCheckinDeviceForUID 从本机客户端 profile 中读取「以 uid 登录的那个
// profile」的签到设备三件套：设备 id 来自该 profile 的 icube-dc 键，brand/type
// 沿用现有探测。没有以 uid 登录的 profile 时回退到 leveldb 扫描（无法区分
// 账号，多账号共用的警告由调用方负责）。
func DetectCheckinDeviceForUID(uid string) (id, brand, typ string, ok bool) {
	if os.Getenv("TW2A_DISABLE_CHECKIN_DETECT") != "" || uid == "" {
		return "", "", "", false
	}
	switch runtime.GOOS {
	case "darwin":
		typ = "mac"
	case "windows":
		typ = "windows"
	default:
		return "", "", "", false
	}
	sawProfile := false
	for _, file := range clientProfileStorageFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue // 该 profile 未安装
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue // 解不出的 profile：下一个可能有（不结束搜索）
		}
		sawProfile = true
		deviceID, profileUID, ok := clientEnvelope(doc)
		if !ok || profileUID != uid {
			continue
		}
		if runtime.GOOS == "darwin" {
			brand = detectBrandDarwin()
		} else {
			brand = detectBrandWindows()
		}
		return deviceID, brand, typ, true
	}
	if sawProfile {
		// 有 profile 但没有以 uid 登录的：该账号保有自己的设备（插件口径，
		// 绝不把别的账号的客户端设备给它——会被上游关联）。
		return "", "", "", false
	}
	// 本机一个 profile 都没有：回退 leveldb 扫描（单账号机器的现状行为）。
	return DetectLocalCheckinDevice()
}

// UgDevice 派生 Trae CN 账号自己的 16 位签到设备 id（插件 ugDevice，#194
// 实测可用）：本机客户端没有以该账号登录时，签到走它。已具客户端形态
// （15/16 位数字）的设备 id 原样返回；否则由登录设备 id 与 uid 派生——
// sha256("trae-cn-device\0{deviceId}\0{uid}") 前 30 个 hex 字符映射到
// [1e15, 1e16)。同一账号每次派生结果相同、绝不轮换，也绝不是别的账号的
// 设备；9095（本设备今日已签）在它上面照旧是当天的答案。登录设备 id 与
// uid 都为空时返回空串。
func UgDevice(deviceID, uid string) string {
	digits := func(s string) bool {
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return s != ""
	}
	if (len(deviceID) == 15 || len(deviceID) == 16) && digits(deviceID) {
		return deviceID
	}
	if deviceID == "" && uid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("trae-cn-device\x00" + deviceID + "\x00" + uid))
	n, _ := new(big.Int).SetString(hex.EncodeToString(sum[:])[:30], 16)
	n.Mod(n, big.NewInt(9_000_000_000_000_000))
	n.Add(n, big.NewInt(1_000_000_000_000_000))
	return n.String()
}
