package middleware

import (
	"bytes"
	"fmt"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func initRequestTimeline(c *gin.Context, timestamp time.Time) {
	if c == nil {
		return
	}
	timeline := logging.NewRequestTimeline()
	c.Set(logging.RequestTimelineGinKey, timeline)
	if timestamp.IsZero() {
		timeline.Add("request.received", "")
		return
	}
	timeline.Add("request.received", "captured_at="+timestamp.Format(time.RFC3339Nano))
}

func addRequestTimelineEvent(c *gin.Context, name string, detail string) {
	requestTimeline(c).Add(name, detail)
}

func addRequestTimelineEventOnce(c *gin.Context, name string, detail string) {
	requestTimeline(c).AddOnce(name, detail)
}

func requestTimeline(c *gin.Context) *logging.RequestTimeline {
	if c == nil {
		return nil
	}
	value, exists := c.Get(logging.RequestTimelineGinKey)
	if !exists {
		return nil
	}
	timeline, _ := value.(*logging.RequestTimeline)
	return timeline
}

func requestTimelineBytes(c *gin.Context) []byte {
	timeline := requestTimeline(c)
	if timeline == nil {
		return nil
	}
	var buf bytes.Buffer
	if errWrite := logging.WriteRequestTimeline(&buf, timeline.Snapshot()); errWrite != nil {
		return []byte(fmt.Sprintf("=== REQUEST TIMELINE ===\nError: %s\n\n", errWrite.Error()))
	}
	return buf.Bytes()
}
