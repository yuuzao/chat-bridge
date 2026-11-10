package httpserver

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"chat_bridge/internal/taskstore"
)

func parseWait(query url.Values) (time.Duration, error) {
	values, ok := query["wait"]
	if !ok {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, statusErr(http.StatusBadRequest, "invalid wait")
	}
	seconds, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || seconds <= 0 || hasExplicitSignOrFraction(values[0]) {
		return 0, statusErr(http.StatusBadRequest, "invalid wait")
	}
	if seconds > int64(math.MaxInt64/int64(time.Second)) {
		return 0, statusErr(http.StatusBadRequest, "invalid wait")
	}
	return time.Duration(seconds) * time.Second, nil
}

func hasExplicitSignOrFraction(raw string) bool {
	if raw == "" {
		return true
	}
	return raw[0] == '+' || raw[0] == '-'
}

func (s *Server) waitView(ctx context.Context, id string, wait time.Duration) (taskstore.View, error) {
	view, err := s.store.View(id)
	if err != nil {
		return taskstore.View{}, err
	}
	if view.Status != taskstore.StatusPending || wait == 0 {
		return view, nil
	}
	timer := time.NewTimer(wait)
	defer stopTimer(timer)
	select {
	case <-view.Done:
	case <-timer.C:
	case <-ctx.Done():
		return taskstore.View{}, ctx.Err()
	}
	return s.store.View(id)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
