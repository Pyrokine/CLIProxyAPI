package executor

import (
	"net/http"
	"strconv"

	"github.com/tidwall/gjson"
)

func parseUpstreamTokenCount(payload []byte, field string) (int64, error) {
	node := gjson.GetBytes(payload, field)
	count, err := strconv.ParseInt(node.Raw, 10, 64)
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() || node.Type != gjson.Number || err != nil ||
		count < 0 {
		return 0, statusErr{
			code: http.StatusBadGateway, msg: "upstream returned invalid token count",
		}.withUpstreamResponse(nil)
	}
	return count, nil
}
