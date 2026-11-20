package executor

func (e *ClaudeExecutor) SupportsApplyPatch() bool       { return e != nil }
func (e *GeminiExecutor) SupportsApplyPatch() bool       { return e != nil }
func (e *GeminiCLIExecutor) SupportsApplyPatch() bool    { return e != nil }
func (e *GeminiVertexExecutor) SupportsApplyPatch() bool { return e != nil }
func (e *AntigravityExecutor) SupportsApplyPatch() bool  { return e != nil }
func (e *AIStudioExecutor) SupportsApplyPatch() bool     { return e != nil }
func (e *OpenAICompatExecutor) SupportsApplyPatch() bool { return e != nil }
func (e *QwenExecutor) SupportsApplyPatch() bool         { return e != nil }
func (e *IFlowExecutor) SupportsApplyPatch() bool        { return e != nil }
func (e *DevinExecutor) SupportsApplyPatch() bool        { return e != nil }

// SupportsApplyPatch 对 Kimi 返回 false，其 Responses 请求使用原生协议，不经过 Claude 的 patch 转换
func (*KimiExecutor) SupportsApplyPatch() bool { return false }
