package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/hologramagentsdk/sdk"
)

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	tp := p.name + "_"

	s.RegisterTool(tp+"ocr_image", sdk.ToolDef{
		Name:        tp + "ocr_image",
		Description: "对图片进行OCR文字识别，支持中文和英文。可传入图片URL或base64编码。返回识别出的文本内容。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"image_url":  map[string]interface{}{"type": "string", "description": "图片的HTTP/HTTPS URL，与 image_data 二选一"},
				"image_data": map[string]interface{}{"type": "string", "description": "图片的base64编码数据（不含 data:image/... 前缀），与 image_url 二选一"},
				"language":   map[string]interface{}{"type": "string", "description": "识别语言，默认 chi_sim+eng（中文简体+英文），可选 chi_sim / eng / chi_sim+eng"},
			},
		},
		Cleaner: func(output string) string {
			var r struct{ Text string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Text != "" {
				return r.Text
			}
			var r2 struct{ Content string }
			if json.Unmarshal([]byte(output), &r2) == nil && r2.Content != "" {
				return r2.Content
			}
			return output
		},
	}, p.handleOcrImage)

	log.Printf("[%s] plugin started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	return nil
}

func (p *Plugin) handleOcrImage(args map[string]interface{}) (interface{}, error) {
	imageURL, _ := args["image_url"].(string)
	imageData, _ := args["image_data"].(string)
	language, _ := args["language"].(string)

	if imageURL == "" && imageData == "" {
		return map[string]interface{}{"error": "请提供 image_url 或 image_data"}, nil
	}

	tmpDir, err := os.MkdirTemp("", "ocr-*")
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("创建临时目录失败: %v", err)}, nil
	}
	defer os.RemoveAll(tmpDir)

	inputPath := filepath.Join(tmpDir, "input.png")

	if imageData != "" {
		data := strings.TrimSpace(imageData)
		if idx := strings.Index(data, "base64,"); idx >= 0 {
			data = data[idx+7:]
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("base64解码失败: %v", err)}, nil
		}
		if err := os.WriteFile(inputPath, decoded, 0644); err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("写入临时文件失败: %v", err)}, nil
		}
	} else {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(imageURL)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("下载图片失败: %v", err)}, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return map[string]interface{}{"error": fmt.Sprintf("下载图片返回状态码 %d", resp.StatusCode)}, nil
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("读取图片数据失败: %v", err)}, nil
		}
		if err := os.WriteFile(inputPath, data, 0644); err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("写入临时文件失败: %v", err)}, nil
		}
	}

	if language == "" {
		language = "chi_sim+eng"
	}

	outputPath := filepath.Join(tmpDir, "output")

	argsList := []string{inputPath, outputPath, "-l", language, "--psm", "3"}
	cmd := exec.Command("tesseract", argsList...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("OCR识别失败: %v (stderr: %s)", err, stderr.String())}, nil
	}

	resultFile := outputPath + ".txt"
	text, err := os.ReadFile(resultFile)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("读取OCR结果失败: %v", err)}, nil
	}

	recognized := strings.TrimSpace(string(text))
	if recognized == "" {
		return map[string]interface{}{"text": "", "message": "未识别出文字内容"}, nil
	}

	return map[string]interface{}{
		"text":      recognized,
		"length":    len(recognized),
		"language":  language,
	}, nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}