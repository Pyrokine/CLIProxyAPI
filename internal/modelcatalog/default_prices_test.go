package modelcatalog

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultPricesMatchCurrentPublishedRates(t *testing.T) {
	t.Parallel()

	cacheWriteSol := 5.0
	cacheWriteTerra := 2.5
	cacheWriteLuna := 0.25
	cacheWriteAstra := 12.5
	cacheWriteCyber := 15.625
	cacheWriteGPT6Sol := 2.5
	cacheWriteGPT61Sol := 2.5
	cacheWriteGPT6Luna := 0.125
	want := map[string]Price{
		"gpt-5.6-sol": {
			Prompt: 4, Completion: 20, Cache: 0.4, CacheWrite: &cacheWriteSol,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-terra": {
			Prompt: 2, Completion: 12, Cache: 0.2, CacheWrite: &cacheWriteTerra,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-luna": {
			Prompt: 0.2, Completion: 1.2, Cache: 0.02, CacheWrite: &cacheWriteLuna,
			Status: PriceStatusPartial,
		},
		"gpt-6-astra": {
			Prompt: 10, Completion: 50, Cache: 1, CacheWrite: &cacheWriteAstra,
			Status: PriceStatusPartial,
		},
		"gpt-5.6-cyber": {
			Prompt: 12.5, Completion: 75, Cache: 1.25, CacheWrite: &cacheWriteCyber,
			Status: PriceStatusPartial,
		},
		"gpt-6-sol": {
			Prompt: 2, Completion: 10, Cache: 0.2, CacheWrite: &cacheWriteGPT6Sol,
			Status: PriceStatusPartial,
		},
		"gpt-6.1-sol": {
			Prompt: 2, Completion: 10, Cache: 0.1, CacheWrite: &cacheWriteGPT61Sol,
			Status: PriceStatusPartial,
		},
		"gpt-6-luna": {
			Prompt: 0.1, Completion: 0.5, Cache: 0.01, CacheWrite: &cacheWriteGPT6Luna,
			Status: PriceStatusPartial,
		},
		"gpt-image-1.5":                     {Status: PriceStatusPartial},
		"gpt-image-2":                       {Status: PriceStatusPartial},
		"gpt-image-2.5-flare":               {Status: PriceStatusPartial},
		"gpt-image-2.5-flare-2026-09-08":    {Status: PriceStatusPartial},
		"gpt-image-2.5-sunburst":            {Status: PriceStatusPartial},
		"gpt-image-2.5-sunburst-2026-09-08": {Status: PriceStatusPartial},
		"claude-fable-5-1": {
			Prompt: 10, Completion: 50, Cache: 0.25, Status: PriceStatusComplete,
		},
		"claude-opus-5-5": {
			Prompt: 4, Completion: 20, Cache: 0.2, Status: PriceStatusComplete,
		},
		"claude-sonnet-5-5": {
			Prompt: 2, Completion: 10, Cache: 0.1, Status: PriceStatusComplete,
		},
		"grok-4.7": {
			Prompt: 2, Completion: 6, Cache: 0.5, Status: PriceStatusPartial,
		},
		"gemini-3.1-flash-image": {
			Status: PriceStatusPartial,
		},
		"gemini-3.1-flash-lite-image": {
			Status: PriceStatusPartial,
		},
		"gemini-3.8-flash": {
			Prompt: 0.75, Completion: 3.75, Cache: 0.075,
			Status: PriceStatusPartial, AutoApply: true,
		},
		"gemini-3.8-flash-high": {Status: PriceStatusUnpriced},
		"deepseek-flash": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusPartial,
		},
		"deepseek-v4-pro": {
			Prompt: 0.66, Completion: 1.98, Cache: 0.022,
			Status: PriceStatusPartial,
		},
		"deepseek-v4-flash": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusPartial,
		},
		"deepseek-v4-flash-vision-exp": {
			Prompt: 0.15, Completion: 0.6, Cache: 0.003,
			Status: PriceStatusPartial,
		},
		"gemini-3.7-flash": {Status: PriceStatusPartial},
		"kimi-k2.7-code": {
			Prompt: 0.95, Completion: 4, Cache: 0.19, Status: PriceStatusPartial,
		},
		"kimi-k2.7-code-highspeed": {
			Prompt: 1.9, Completion: 8, Cache: 0.38, Status: PriceStatusPartial,
		},
		"kimi-k3": {
			Prompt: 3, Completion: 15, Cache: 0.3,
			Status: PriceStatusPartial, AutoApply: true,
		},
	}
	prices := DefaultPrices()
	for name, expected := range want {
		got, ok := prices[name]
		if !ok {
			t.Errorf("DefaultPrices()[%q] missing", name)
			continue
		}
		if got.Prompt != expected.Prompt || got.Completion != expected.Completion || got.Cache != expected.Cache ||
			got.EffectiveStatus() != expected.EffectiveStatus() ||
			got.AutoApply != expected.AutoApply ||
			!reflect.DeepEqual(got.CacheWrite, expected.CacheWrite) {
			t.Errorf("DefaultPrices()[%q] = %+v, want core fields %+v", name, got, expected)
		}
		if expected.EffectiveStatus() == PriceStatusUnpriced {
			if got.HasReferenceTokenRates() || got.UnpricedReason == "" {
				t.Errorf(
					"DefaultPrices()[%q] must explain unpriced status without numeric rates: %+v",
					name,
					got,
				)
			}
		} else if got.HasBaseTokenRates() && (got.CacheRead == nil || *got.CacheRead != expected.Cache) {
			t.Errorf("DefaultPrices()[%q].CacheRead = %v, want %v", name, got.CacheRead, expected.Cache)
		}
		if got.Currency != "USD" || got.Source == "" || got.CheckedAt == "" {
			t.Errorf("DefaultPrices()[%q] source metadata = %+v", name, got)
		}
	}

	metadata := map[string]struct {
		source      string
		checkedAt   string
		limitations []string
	}{
		"gpt-6-astra": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
				"当前精确型号页与公开 Codex 目录列出该 ID；额外 API 访问资格条件未取得明确说明，旧 Trusted Access 限制保留为待核实事实",
			},
		},
		"gpt-5.6-cyber": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"须启用 Daybreak Red；Standard 短档输入 12.5、输出 75、缓存读 1.25、30m 写 15.625、12h 写 31.25",
				"当前主价目表与精确模型页的 1.25x 写入倍率一致，原 USD 15 冲突说明应删除",
				"精确页列最大输入 272000，也列 >272000 时输入 2x/输出 1.5x 条件；主价目表未公布该型号长档数值和长档缓存价，当前 schema 不推算缺失项",
			},
		},
		"gpt-6-sol": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
			},
		},
		"gpt-6.1-sol": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
			},
		},
		"claude-sonnet-5-5": {
			source:    "https://platform.claude.com/docs/en/about-claude/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard API 参考价；Batch、Fast、US-only inference 1.1x、合作云平台与工具费另计",
			},
		},
		"gpt-6-luna": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
			},
		},
		"gpt-image-1.5": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-1.5",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"精确官方模型页标为 Deprecated，当前参考价与可用性状态分别使用",
			},
		},
		"gpt-image-2": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-2",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"缓存价只适用于 Responses API 的 image-generation tool，不适用于直接 Images API generations/edits",
				"精确官方模型页标为 Deprecated，当前参考价与可用性状态分别使用",
			},
		},
		"gpt-image-2.5-flare": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-2.5-flare",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"缓存价只适用于 Responses API 的 image-generation tool，不适用于直接 Images API generations/edits",
			},
		},
		"gpt-image-2.5-flare-2026-09-08": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-2.5-flare",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"缓存价只适用于 Responses API 的 image-generation tool，不适用于直接 Images API generations/edits",
			},
		},
		"gpt-image-2.5-sunburst": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"缓存价只适用于 Responses API 的 image-generation tool，不适用于直接 Images API generations/edits",
			},
		},
		"gpt-image-2.5-sunburst-2026-09-08": {
			source:    "https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst",
			checkedAt: "2026-11-18",
			limitations: []string{
				"同一请求的 text/image 输入、缓存读取和图像输出分别定价；缺少模态或缓存计量时保留对应未计价部分",
				"图像使用 USD / 1M image tokens；未将每张图的估算单价写成固定 token 价格",
				"缓存价只适用于 Responses API 的 image-generation tool，不适用于直接 Images API generations/edits",
			},
		},
		"gemini-3.8-flash": {
			source:    "https://aistudio.google.com/docs/pricing#gemini-3.8-flash",
			checkedAt: "2026-11-18",
			limitations: []string{
				"仅 Gemini Developer API Standard 付费档参考，Batch/Flex/Priority、工具和缓存存储按 published_tables 的原始单位单列",
				"当前缓存存储 USD 0.50 / 1,000,000 token / hour；2027-01-01 为 USD 1.00 / 1,000,000 token / hour",
				"Input price: $0.75 through December 31, 2026.; $1.50 starting January 1, 2027.",
				"Output price (including thinking tokens): $3.75 through December 31, 2026.; $7.50 starting January 1, 2027.",
				"Context caching price: $0.075 through December 31, 2026.; $0.15 starting January 1, 2027.; $0.50 / 1,000,000 tokens per hour (storage price) through December 31, 2026.; $1.00 / 1,000,000 tokens per hour (storage price) starting January 1, 2027.",
			},
		},
		"gemini-3.8-flash-high": {
			source:    "https://antigravity.google/docs/plans",
			checkedAt: "2026-11-18",
			limitations: []string{
				"没有公开证据把该 thinking 后缀或 agent 别名定义为独立 Gemini API ID；也未取得 Antigravity 精确模型按 token 价表，停止继承主站基础型号费率",
			},
		},
		"kimi-k2.7-code": {
			source:    "https://platform.kimi.ai/docs/pricing/chat",
			checkedAt: "2026-11-18",
			limitations: []string{
				"仅用于精确同名 Moonshot/Kimi 开放平台按量 API 的单价参考；Kimi Code 订阅额度按独立合同消耗",
				"每 1 MTok = 1,000,000 tokens；USD 国际站与 CNY 国内站分别列示，未进行汇率换算",
				"国际站价格不含适用税费，税额按司法辖区在结算时计算",
				"K2 系列只向已有合资格账户提供；价格页称自 2026-10-31 停止新用户，模型/API 参数页称 2026-11-01 之前注册用户可继续，计划 2026-12 底下线",
				"当前标准价目未发布 K2 独立缓存写入费，不从 K3 或其他型号推算",
				"CPA 内置 Kimi 渠道同名兼容 ID 当前转为 kimi-for-coding，即 K2.8 Preview；该渠道的实际按 token 价格保持未知，不能使用 API K2.7 费率描述该订阅调用",
			},
		},
		"kimi-k2.7-code-highspeed": {
			source:    "https://platform.kimi.ai/docs/pricing/chat",
			checkedAt: "2026-11-18",
			limitations: []string{
				"仅用于精确同名 Moonshot/Kimi 开放平台按量 API 的单价参考；Kimi Code 订阅额度按独立合同消耗",
				"每 1 MTok = 1,000,000 tokens；USD 国际站与 CNY 国内站分别列示，未进行汇率换算",
				"国际站价格不含适用税费，税额按司法辖区在结算时计算",
				"K2 系列只向已有合资格账户提供；价格页称自 2026-10-31 停止新用户，模型/API 参数页称 2026-11-01 之前注册用户可继续，计划 2026-12 底下线",
				"当前标准价目未发布 K2 独立缓存写入费，不从 K3 或其他型号推算",
			},
		},
		"grok-4.7": {
			source:    "https://docs.x.ai/developers/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"基础费率仅适用于总 prompt token 数小于 200000 的 xAI 第一方全局 Standard API 请求",
				"总 prompt token 包含缓存命中 token；达到 200000 后整次请求所有 token 使用长上下文费率",
				"US endpoint 的输入、缓存输入、输出费率乘以 1.1；第三方渠道和消费订阅费用独立",
				"长上下文按公开的整次请求档位计价，未知计量保留具体未计价原因",
				"Batch 表列出的七个 4.x ID 享 20% token 折扣；Build 不在折扣表中",
				"Long-context reference (USD per million tokens): {\"trigger\":\"prompt_tokens >= 200000, including cached prompt tokens\",\"rates\":{\"prompt\":4.0,\"cache_read\":1.0,\"completion\":12.0},\"applies_to\":\"all request input, cached input and output tokens\"}",
				"Checked against official-site search-index excerpts on 2026-11-18; the full official page was unavailable during this review.",
			},
		},
		"gpt-6.2-astra": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
				"默认速度费率按非优惠标准维护，API 与 Codex 分别采用对应渠道的公开倍率；Flex 费率未公布",
			},
		},
		"gpt-6.2-sol": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
				"默认速度费率按非优惠标准维护，API 与 Codex 分别采用对应渠道的公开倍率；Flex 费率未公布",
			},
		},
		"gpt-6.1-terra": {
			source:    "https://developers.openai.com/api/docs/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard 参考价；工具费与区域附加费另计",
				"超过 272000 输入 tokens 时整次请求使用长上下文档",
				"30 分钟与 12 小时缓存写入使用各自公布的单价",
			},
		},
		"claude-fable-5-2": {
			source:    "https://platform.claude.com/docs/en/about-claude/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard API 参考价；Batch、Fast、US-only inference 1.1x、合作云平台与工具费另计",
			},
		},
		"claude-haiku-5-5": {
			source:    "https://platform.claude.com/docs/en/about-claude/pricing",
			checkedAt: "2026-11-18",
			limitations: []string{
				"USD / 1M tokens，第一方 global Standard API 参考价；Batch、Fast、US-only inference 1.1x、合作云平台与工具费另计",
				"以上是总 prompt 长度不超过 100000 tokens 的短档；计数包含 cache read/write，超过阈值整次请求改为输入 0.50、输出 2.50、读 0.05、5m 写 0.625、1h 写 1",
			},
		},
	}
	for name, expected := range metadata {
		got := prices[name]
		if got.Source != expected.source || got.CheckedAt != expected.checkedAt ||
			!reflect.DeepEqual(got.Limitations, expected.limitations) {
			t.Errorf(
				"DefaultPrices()[%q] metadata = %+v, want source %q and limitations %v",
				name,
				got,
				expected.source,
				expected.limitations,
			)
		}
	}
	for _, name := range []string{"gemini-3.6-flash", "gemini-3.8-flash"} {
		got := prices[name]
		if got.EffectiveUntil != "" {
			t.Errorf("DefaultPrices()[%q] expires before its published future rate", name)
		}
		foundFutureRate := false
		for _, rule := range got.ConditionalRates {
			if rule.Provider == "gemini" && rule.EffectiveFrom == "2027-01-01T00:00:00Z" && rule.Speed == "" && rule.Prompt != nil && rule.Completion != nil && rule.CacheRead != nil {
				foundFutureRate = *rule.Prompt == 1.5 && *rule.Completion == 7.5 && *rule.CacheRead == 0.15
			}
		}
		if !foundFutureRate {
			t.Errorf("DefaultPrices()[%q] lacks its exact published 2027 rates", name)
		}
	}

	for _, name := range []string{
		"k2.8",
		"kimi-k2.8-preview",
		"k2.8-preview",
		"k2.8-code",
	} {
		if price, ok := prices[name]; ok {
			t.Errorf("DefaultPrices()[%q] = %+v, want removed", name, price)
		}
	}

	for _, name := range []string{
		"grok-imagine-image",
		"grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"grok-imagine-video",
		"grok-imagine-video-1.5",
		"qwen3-coder-next",
		"glm-4-voice",
		"glm-4.1v-thinking-flash",
		"glm-4.1v-thinking-flashx",
		"glm-4v-flash",
		"glm-5v-turbo",
		"autoglm-phone",
	} {
		price, ok := prices[name]
		checkedAt := "2026-11-18"
		if len(price.ModalityRates) > 0 || len(price.ChargeRates) > 0 {
			if price.EffectiveStatus() != PriceStatusPartial || price.HasBaseTokenRates() || price.Source == "" || price.CheckedAt != checkedAt || !price.RequestScoped {
				t.Errorf("published conditional price %q = %+v", name, price)
			}
			continue
		}
		if !ok || price.EffectiveStatus() != PriceStatusUnpriced || price.HasReferenceTokenRates() ||
			price.Source == "" || price.CheckedAt != checkedAt || price.UnpricedReason == "" {
			t.Errorf("DefaultPrices()[%q] = %+v, %t, want sourced unpriced entry", name, price, ok)
		}
	}

	fable := prices["claude-fable-5-1"]
	if fable.CacheWrite5m == nil || *fable.CacheWrite5m != 12.5 ||
		fable.CacheWrite1h == nil || *fable.CacheWrite1h != 20 {
		t.Fatalf("Claude Fable 5.1 cache-write rates = (%v, %v)", fable.CacheWrite5m, fable.CacheWrite1h)
	}
	opus := prices["claude-opus-5-5"]
	if opus.CacheWrite5m == nil || *opus.CacheWrite5m != 5 ||
		opus.CacheWrite1h == nil || *opus.CacheWrite1h != 8 {
		t.Fatalf("Claude Opus 5.5 cache-write rates = (%v, %v)", opus.CacheWrite5m, opus.CacheWrite1h)
	}

	// This contract test intentionally compares a compile-time manifest version.
	// noinspection GoBoolExpressions
	if DefaultsVersion != "2026-10-09.1" {
		t.Fatalf("DefaultsVersion = %q, want 2026-10-09.1", DefaultsVersion)
	}
}

func TestDeepSeekDefaultPricesSelectRecurringUTCBands(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("test fixture weekday = %s", monday.Weekday())
	}
	models := []struct {
		name    string
		offPeak tokenRates
		peak    tokenRates
	}{
		{
			name:    "deepseek-flash",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
		{
			name:    "deepseek-v4-pro",
			offPeak: tokenRates{Prompt: 0.66, Completion: 1.98, Cache: 0.022},
			peak:    tokenRates{Prompt: 1.32, Completion: 3.96, Cache: 0.044},
		},
		{
			name:    "deepseek-v4-flash",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
		{
			name:    "deepseek-v4-flash-vision-exp",
			offPeak: tokenRates{Prompt: 0.15, Completion: 0.6, Cache: 0.003},
			peak:    tokenRates{Prompt: 0.3, Completion: 1.2, Cache: 0.006},
		},
	}
	times := []struct {
		name string
		at   time.Time
		peak bool
	}{
		{name: "weekday off-peak before first band", at: monday.Add(59 * time.Minute)},
		{name: "weekday first peak band", at: monday.Add(time.Hour), peak: true},
		{name: "weekday gap starts at 04:00", at: monday.Add(4 * time.Hour)},
		{name: "weekday second peak band", at: monday.Add(6 * time.Hour), peak: true},
		{name: "weekday peak end is exclusive at 10:00", at: monday.Add(10 * time.Hour)},
		{name: "weekend is off-peak", at: monday.AddDate(0, 0, 5).Add(2 * time.Hour)},
	}

	prices := DefaultPrices()
	for _, model := range models {
		t.Run(
			model.name, func(t *testing.T) {
				price, ok := prices[model.name]
				if !ok {
					t.Fatalf("missing price for %s", model.name)
				}
				for _, test := range times {
					t.Run(
						test.name, func(t *testing.T) {
							want := model.offPeak
							if test.peak {
								want = model.peak
							}
							selected := price.RatesAt(test.at)
							cacheRead, hasCacheRead := selected.CacheReadRate()
							if !hasCacheRead || selected.Prompt != want.Prompt ||
								selected.Completion != want.Completion || cacheRead != want.Cache {
								t.Fatalf(
									"RatesAt(%s) = %+v, cache_read=%v/%t; want %+v",
									test.at.Format(time.RFC3339),
									selected,
									cacheRead,
									hasCacheRead,
									want,
								)
							}
						},
					)
				}
			},
		)
	}
}

func TestDefaultInventoryHasExplicitPriceStatus(t *testing.T) {
	t.Parallel()

	catalog := DefaultFile()
	for _, model := range catalog.Models {
		price, ok := catalog.Price(model.Name)
		if !ok {
			t.Errorf("default inventory model %q has no price-status record", model.Name)
			continue
		}
		switch price.EffectiveStatus() {
		case PriceStatusComplete, PriceStatusPartial, PriceStatusUnpriced:
		default:
			t.Errorf(
				"default inventory model %q has invalid price status %q",
				model.Name,
				price.EffectiveStatus(),
			)
		}
	}
}

func TestDefaultCatalogKeepsPriceManifestSeparateFromInventory(t *testing.T) {
	t.Parallel()

	catalog := DefaultFile()
	aliases := catalog.ToOAuthModelAlias()
	for _, name := range []string{"gpt-5", "gpt-5.6-cyber"} {
		if _, ok := catalog.Price(name); !ok {
			t.Errorf("default price manifest missing %q", name)
		}
		if _, ok := catalog.Find(name); ok {
			t.Errorf("price-only record %q created an inventory model", name)
		}
		for channel, entries := range aliases {
			for _, entry := range entries {
				if modelKey(entry.Name) == modelKey(name) || modelKey(entry.Alias) == modelKey(name) {
					t.Errorf(
						"price-only record %q created OAuth alias in channel %q: %+v",
						name,
						channel,
						entry,
					)
				}
			}
		}
	}
	astra, ok := catalog.Find("gpt-6-astra")
	if !ok || astra.Provider != "GPT" || astra.Channel != "codex" || astra.Group != "GPT 6" || !astra.Enabled {
		t.Fatalf("default Astra inventory model = %+v, %v", astra, ok)
	}
	if _, ok := catalog.Price("gpt-6-astra"); !ok {
		t.Fatal("default Astra price is missing")
	}
	sonnet, ok := catalog.Find("claude-sonnet-5-5")
	if !ok || sonnet.Provider != "Claude" || sonnet.Channel != "claude" || !sonnet.Enabled {
		t.Fatalf("default Sonnet 5.5 inventory model = %+v, %v", sonnet, ok)
	}
	for _, name := range []string{"glm-5-turbo", "qwen3-max-preview"} {
		price, ok := catalog.Price(name)
		if !ok || price.EffectiveStatus() != PriceStatusUnpriced || price.UnpricedReason == "" {
			t.Errorf("unpriced record %q = %+v, %v", name, price, ok)
		}
	}
	if _, exists := catalog.Find("gpt-6.1-sol"); !exists {
		t.Fatal("declared Sol inventory entry missing from the embedded model catalog")
	}

}

func TestDefaultUpdateAppliesInventoryAndPriceManifestIndependently(t *testing.T) {
	t.Parallel()

	const previousDefaultsVersion = "2026-08-26"
	current := DefaultFile()
	current.DefaultsVersion = previousDefaultsVersion
	current.DeletePrice("gpt-5")
	current.SetPrice("gpt-5.6-sol", Price{Prompt: 5, Completion: 30, Cache: 0.5})
	current.DeletePrice("gpt-6-astra")
	current.SetPrice("gpt-5.6-cyber", Price{Prompt: 1, Completion: 2, Cache: 0.1})
	models := current.Models[:0]
	for _, model := range current.Models {
		if model.Name != "gemini-3.7-flash" {
			models = append(models, model)
		}
	}
	current.Models = models

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, current); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate() error = %v", err)
	}
	changes := make(map[string]defaultUpdateChange, len(preview.Changes))
	for _, change := range preview.Changes {
		changes[change.Name] = change
	}
	for _, name := range []string{"gpt-5", "gpt-5.6-sol", "gpt-6-astra", "gpt-5.6-cyber", "gemini-3.7-flash"} {
		if _, ok := changes[name]; !ok {
			t.Errorf("default update missing %q: %#v", name, preview.Changes)
		}
	}

	decisions := make([]ApplyDecision, 0, len(changes))
	for name := range changes {
		decisions = append(decisions, ApplyDecision{Name: name, Action: "use_default"})
	}
	result, err := applyDefaultUpdate(configPath, decisions)
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate() error = %v", err)
	}
	if result.Applied != len(changes) || result.BackupPath == "" {
		t.Fatalf("ApplyDefaultUpdate() = %#v, changes=%d", result, len(changes))
	}
	updated, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() updated catalog error = %v", err)
	}
	if updated.DefaultsVersion != DefaultsVersion || updated.SchemaVersion != SchemaVersion {
		t.Fatalf("updated versions = (%d, %q)", updated.SchemaVersion, updated.DefaultsVersion)
	}
	defaults := DefaultFile()
	for _, name := range []string{"gpt-5", "gpt-5.6-cyber"} {
		if _, ok := updated.Find(name); ok {
			t.Fatalf("price-only default %q became inventory during update", name)
		}
		got, gotOK := updated.Price(name)
		want, wantOK := defaults.Price(name)
		if !gotOK || !wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("updated price-only default %q = %+v, %v; want %+v, %v", name, got, gotOK, want, wantOK)
		}
	}
	astra, ok := updated.Find("gpt-6-astra")
	if !ok || astra.Provider != "GPT" || astra.Channel != "codex" || astra.Group != "GPT 6" || !astra.Enabled {
		t.Fatalf("updated Astra inventory model = %+v, %v", astra, ok)
	}
	gotAstraPrice, gotAstraPriceOK := updated.Price("gpt-6-astra")
	wantAstraPrice, wantAstraPriceOK := defaults.Price("gpt-6-astra")
	if !gotAstraPriceOK || !wantAstraPriceOK || !reflect.DeepEqual(gotAstraPrice, wantAstraPrice) {
		t.Fatalf(
			"updated Astra price = %+v, %v; want %+v, %v",
			gotAstraPrice,
			gotAstraPriceOK,
			wantAstraPrice,
			wantAstraPriceOK,
		)
	}
	if _, ok := updated.Find("gemini-3.7-flash"); !ok {
		t.Fatal("new inventory model was not applied")
	}
	if price, ok := updated.Price("gpt-5.6-sol"); !ok || price.Prompt != 4 {
		t.Fatalf("updated gpt-5.6-sol price = %+v, %v", price, ok)
	}
	backupData, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup error = %v", err)
	}
	backup, err := decodeFile(backupData)
	if err != nil {
		t.Fatalf("decode backup error = %v", err)
	}
	if backup.DefaultsVersion != previousDefaultsVersion {
		t.Fatalf("backup defaults version = %q", backup.DefaultsVersion)
	}
	if price, ok := backup.Price("gpt-5.6-sol"); !ok || price.Prompt != 5 {
		t.Fatalf("backup gpt-5.6-sol price = %+v, %v", price, ok)
	}
}

func TestDefaultUpdateRemovesStaleInventoryWithoutDeletingPriceOnlyDefault(t *testing.T) {
	t.Parallel()

	const name = "gpt-5"
	current := DefaultFile()
	current.DefaultsVersion = "2026-08-26"
	if _, ok := current.Find(name); ok {
		t.Fatalf("%s unexpectedly exists in default inventory", name)
	}
	defaultPrice, ok := current.Price(name)
	if !ok {
		t.Fatalf("default price missing for %s", name)
	}
	current.Models = append(
		current.Models, Model{
			Name:        name,
			Provider:    "OpenAI",
			Channel:     "codex",
			Group:       "GPT 5",
			UserCreated: false,
			Enabled:     true,
		},
	)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := Save(configPath, current); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	result, err := applyDefaultUpdate(configPath, []ApplyDecision{{Name: name, Action: "remove_default"}})
	if err != nil {
		t.Fatalf("ApplyDefaultUpdate() error = %v", err)
	}
	if result.Applied != 1 {
		t.Fatalf("ApplyDefaultUpdate() applied = %d, want 1", result.Applied)
	}
	updated, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() updated catalog error = %v", err)
	}
	if _, ok := updated.Find(name); ok {
		t.Fatalf("stale inventory model %s was not removed", name)
	}
	price, ok := updated.Price(name)
	if !ok || !reflect.DeepEqual(price, defaultPrice) {
		t.Fatalf("price-only default %s = %+v, %v; want %+v", name, price, ok, defaultPrice)
	}
	preview, err := PreviewDefaultUpdate(configPath)
	if err != nil {
		t.Fatalf("PreviewDefaultUpdate() error = %v", err)
	}
	if len(preview.Changes) != 0 {
		t.Fatalf("default update left changes: %#v", preview.Changes)
	}
}
