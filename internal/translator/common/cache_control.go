package common

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AttachMessageCacheControl 将消息级缓存标记写到最后一个内容块
func AttachMessageCacheControl(msg []byte, src gjson.Result) []byte {
	content := gjson.GetBytes(msg, "content").Array()
	if len(content) == 0 {
		return msg
	}
	last := content[len(content)-1]
	if last.Get("cache_control").Exists() || last.Get("type").String() == "thinking" {
		return msg
	}
	updated := AttachCacheControl([]byte(last.Raw), src)
	out, err := sjson.SetRawBytes(msg, "content."+strconv.Itoa(len(content)-1), updated)
	if err != nil {
		return msg
	}
	return out
}

// AttachToolMessageCacheControl 将首个有效内容标记或消息标记写到 tool_result 块
func AttachToolMessageCacheControl(msg []byte, src gjson.Result) []byte {
	for index, block := range gjson.GetBytes(msg, "content").Array() {
		if block.Get("type").String() != "tool_result" {
			continue
		}
		updated := []byte(block.Raw)
		content := src.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, part gjson.Result) bool {
				updated = AttachCacheControl(updated, part)
				return !gjson.GetBytes(updated, "cache_control").Exists()
			})
		} else if content.IsObject() {
			updated = AttachCacheControl(updated, content)
		}
		if !gjson.GetBytes(updated, "cache_control").Exists() {
			updated = AttachCacheControl(updated, src)
		}
		out, err := sjson.SetRawBytes(msg, "content."+strconv.Itoa(index), updated)
		if err != nil {
			return msg
		}
		return out
	}
	return msg
}

// AttachCacheControl copies a Claude-compatible cache_control object from src onto dst.
func AttachCacheControl(dst []byte, src gjson.Result) []byte {
	cacheControl := src.Get("cache_control")
	if !cacheControl.IsObject() || cacheControl.Get("type").Type != gjson.String ||
		cacheControl.Get("type").String() != "ephemeral" {
		return dst
	}
	out, err := sjson.SetRawBytes(dst, "cache_control", []byte(cacheControl.Raw))
	if err != nil {
		return dst
	}
	return out
}
