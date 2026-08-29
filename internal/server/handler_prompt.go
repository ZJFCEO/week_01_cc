package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"llmgateway/internal/apierr"
	"llmgateway/internal/llm"
	"llmgateway/internal/prompt"
)

// createPromptBody 是新建模板版本的请求体。
type createPromptBody struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	System      string        `json:"system"`
	Messages    []llm.Message `json:"messages"`
}

// handleCreatePrompt 新建一个模板版本。
// 同名模板不会被覆盖：每次 POST 都追加一个新版本，旧版本永远可以被引用。
func (s *Server) handleCreatePrompt(c *gin.Context) {
	var body createPromptBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, apierr.New(apierr.CodeInvalidRequest, "请求体不是合法 JSON: %s", err.Error()))
		return
	}
	t, err := s.prompts.Create(body.Name, body.Description, body.System, body.Messages)
	if err != nil {
		abortErr(c, apierr.From(err))
		return
	}
	c.JSON(http.StatusCreated, t)
}

// handleListPrompts 列出全部模板（含最新版本号与变量列表）。
func (s *Server) handleListPrompts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"prompts": s.prompts.List()})
}

// parseVersion 解析 ?version=N；省略或非法时返回 0，表示取 latest。
func parseVersion(c *gin.Context) int {
	raw := c.Query("version")
	if raw == "" || raw == "latest" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// handleGetPrompt 取某个模板的指定版本。
func (s *Server) handleGetPrompt(c *gin.Context) {
	t, err := s.prompts.Get(c.Param("name"), parseVersion(c))
	if err != nil {
		abortErr(c, apierr.From(err))
		return
	}
	c.JSON(http.StatusOK, t)
}

// handlePromptVersions 列出某模板的全部历史版本，用于对比不同版本的提示词。
func (s *Server) handlePromptVersions(c *gin.Context) {
	list, err := s.prompts.Versions(c.Param("name"))
	if err != nil {
		abortErr(c, apierr.From(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": c.Param("name"), "versions": list})
}

// renderBody 是渲染预览的请求体。
type renderBody struct {
	Version   int               `json:"version"`
	Variables map[string]string `json:"variables"`
}

// handleRenderPrompt 只做变量替换预览，不调用模型。
// 用来在真正花 Token 之前确认模板渲染结果，也是验证「变量替换」的最直接证据。
func (s *Server) handleRenderPrompt(c *gin.Context) {
	var body renderBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, apierr.New(apierr.CodeInvalidRequest, "请求体不是合法 JSON: %s", err.Error()))
		return
	}
	// ① 先按 name + version 取到不可变的模板快照
	t, err := s.prompts.Get(c.Param("name"), body.Version)
	if err != nil {
		abortErr(c, apierr.From(err))
		return
	}
	// ② 再做替换；缺变量会返回 PROMPT_VAR_MISSING 而不是留下 {{xxx}}
	rendered, err := prompt.Render(t, body.Variables)
	if err != nil {
		abortErr(c, apierr.From(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":     t.Name,
		"version":  t.Version,
		"system":   rendered.System,
		"messages": rendered.Messages,
	})
}
