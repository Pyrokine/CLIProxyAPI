package common

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AttachCacheControl copies a Claude-compatible cache_control object from src onto dst.
func AttachCacheControl(dst []byte, src gjson.Result) []byte {
	cacheControl := src.Get("cache_control")
	if !cacheControl.Exists() || cacheControl.Type == gjson.Null || !cacheControl.IsObject() {
		return dst
	}
	out, err := sjson.SetRawBytes(dst, "cache_control", []byte(cacheControl.Raw))
	if err != nil {
		return dst
	}
	return out
}
