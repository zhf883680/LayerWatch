package vision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tempUploader 阿里云百炼临时文件上传：把本地图片上传到免费临时 OSS，拿到 oss:// URL 供视觉模型引用。
// 相比把 base64 塞进请求体，走临时 URL 的请求体小得多（省带宽、省日志、便于复用同一帧）。
// 官方文档：https://help.aliyun.com/zh/model-studio/get-temporary-file-url
// 注意：临时 URL 有效期 48h，仅适合个人/测试，请勿用于生产或高并发。
type tempUploader struct {
	baseURL string // 兼容模式 base，如 https://dashscope.aliyuncs.com/compatible-mode/v1
	apiKey  string
	model   string // 上传的文件须与调用模型一致
	client  *http.Client

	mu    sync.Mutex
	cache map[string]string // 图片内容 sha256 -> oss:// URL（复用同一帧，避免反复上传）
	order []string          // 简单 LRU：记录插入顺序
	cap   int
}

func newTempUploader(baseURL, apiKey, model string, client *http.Client) *tempUploader {
	if client == nil {
		client = http.DefaultClient
	}
	return &tempUploader{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  client,
		cache:   map[string]string{},
		cap:     128,
	}
}

// upload 上传/复用一张图，返回 oss:// URL。
func (u *tempUploader) upload(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	u.mu.Lock()
	if url, ok := u.cache[key]; ok {
		u.mu.Unlock()
		return url, nil
	}
	u.mu.Unlock()

	url, err := u.uploadFresh(data)
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	u.cache[key] = url
	u.order = append(u.order, key)
	if len(u.order) > u.cap {
		old := u.order[0]
		u.order = u.order[1:]
		delete(u.cache, old)
	}
	u.mu.Unlock()
	return url, nil
}

// uploadFresh 真正执行 getPolicy -> 上传 OSS -> 返 oss:// URL。
func (u *tempUploader) uploadFresh(data []byte) (string, error) {
	policy, err := u.getPolicy()
	if err != nil {
		return "", err
	}
	// key = upload_dir + 文件名（带扩展名）
	ext := ".jpg"
	if m := sniffImageMIME(data); m == "image/png" {
		ext = ".png"
	} else if m == "image/webp" {
		ext = ".webp"
	}
	fileName := fmt.Sprintf("layerwatch_%d%s", time.Now().UnixNano(), ext)
	key := strings.TrimSuffix(policy.UploadDir, "/") + "/" + fileName

	if err := u.postToOSS(policy, key, fileName, data); err != nil {
		return "", err
	}
	return "oss://" + key, nil
}

type uploadPolicy struct {
	UploadDir           string `json:"upload_dir"`
	UploadHost          string `json:"upload_host"`
	Policy              string `json:"policy"`
	Signature           string `json:"signature"`
	OSSAccessKeyID      string `json:"oss_access_key_id"`
	XOSSObjectACL       string `json:"x_oss_object_acl"`
	XOSSForbidOverwrite string `json:"x_oss_forbid_overwrite"`
}

func (u *tempUploader) getPolicy() (*uploadPolicy, error) {
	endpoint := strings.TrimRight(u.baseURL, "/")
	// 把 /compatible-mode/v1 替换成 /api/v1，拼 /uploads
	endpoint = strings.Replace(endpoint, "/compatible-mode/v1", "/api/v1", 1)
	url := endpoint + "/uploads"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.apiKey)
	req.Header.Set("Content-Type", "application/json")
	q := req.URL.Query()
	q.Set("action", "getPolicy")
	q.Set("model", u.model)
	req.URL.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("getPolicy http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var parsed struct {
		Data uploadPolicy `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Data.UploadHost == "" || parsed.Data.Policy == "" {
		return nil, fmt.Errorf("getPolicy 返回缺少字段")
	}
	return &parsed.Data, nil
}

func (u *tempUploader) postToOSS(p *uploadPolicy, key, fileName string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"OSSAccessKeyId":         p.OSSAccessKeyID,
		"Signature":              p.Signature,
		"policy":                 p.Policy,
		"x-oss-object-acl":       p.XOSSObjectACL,
		"x-oss-forbid-overwrite": p.XOSSForbidOverwrite,
		"key":                    key,
		"success_action_status":  "200",
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, p.UploadHost, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("上传 OSS http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
