package middleware

import (
	"bytes"
	"fmt"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

const routeTraceStreamTerminalWait = 6 * time.Second

func initRequestTimeline(c *gin.Context, timestamp time.Time) {
	if c == nil {
		return
	}
	timeline := logging.NewRequestTimeline()
	c.Set(logging.RequestTimelineGinKey, timeline)
	c.Set(logging.RouteTraceGinKey, logging.NewRouteTrace())
	if timestamp.IsZero() {
		timeline.Add("request.received", "")
		return
	}
	timeline.Add("request.received", "captured_at="+timestamp.Format(time.RFC3339Nano))
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
	trace := requestRouteTrace(c)
	if timeline == nil && trace == nil {
		return nil
	}
	var buf bytes.Buffer
	if timeline != nil {
		if errWrite := logging.WriteRequestTimeline(&buf, timeline.Snapshot()); errWrite != nil {
			_, _ = fmt.Fprintf(&buf, "=== REQUEST TIMELINE ===\nError: %s\n\n", errWrite.Error())
		}
	}
	if trace != nil {
		trace.WaitForStreamTerminal(routeTraceStreamTerminalWait)
		if errWrite := logging.WriteRouteTrace(&buf, trace.Snapshot()); errWrite != nil {
			_, _ = fmt.Fprintf(&buf, "=== ROUTE TRACE ===\nError: %s\n\n", errWrite.Error())
		}
	}
	return buf.Bytes()
}

func requestRouteTrace(c *gin.Context) *logging.RouteTrace {
	if c == nil {
		return nil
	}
	value, exists := c.Get(logging.RouteTraceGinKey)
	if !exists {
		return nil
	}
	trace, _ := value.(*logging.RouteTrace)
	return trace
}
