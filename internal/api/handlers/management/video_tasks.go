package management

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

func videoTaskStateKey(provider, id string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return "video-task:" + encode([]byte(provider)) + "." + encode([]byte(id))
}

func (h *Handler) CheckVideoTaskStorage(ctx context.Context) error {
	if h == nil || h.stateStore == nil || h.stateStore.db == nil {
		return errors.New("video ownership storage is unavailable")
	}
	return h.stateStore.db.PingContext(ctx)
}

func (h *Handler) LoadVideoTask(ctx context.Context, provider, id string) (handlers.VideoTask, bool, error) {
	if err := h.CheckVideoTaskStorage(ctx); err != nil {
		return handlers.VideoTask{}, false, err
	}
	var payload []byte
	err := h.stateStore.db.QueryRowContext(
		ctx, "SELECT value FROM state_items WHERE key = ?", videoTaskStateKey(provider, id),
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return handlers.VideoTask{}, false, nil
	}
	if err != nil {
		return handlers.VideoTask{}, false, fmt.Errorf("read video ownership: %w", err)
	}
	var task handlers.VideoTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return handlers.VideoTask{}, false, fmt.Errorf("decode video ownership: %w", err)
	}
	if task.Provider != provider || task.ID != id || task.OwnerIdentity == "" || task.AuthID == "" || task.Model == "" {
		return handlers.VideoTask{}, false, errors.New("video ownership record is invalid")
	}
	return task, true, nil
}

func (h *Handler) SaveVideoTask(ctx context.Context, task handlers.VideoTask) error {
	if err := h.CheckVideoTaskStorage(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Provider) == "" ||
		strings.TrimSpace(
			task.OwnerIdentity,
		) == "" || strings.TrimSpace(task.AuthID) == "" || strings.TrimSpace(task.Model) == "" {
		return errors.New("video ownership record is incomplete")
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	result, err := h.stateStore.db.ExecContext(
		ctx,
		"INSERT INTO state_items(key, value, updated_at) VALUES(?, ?, ?) ON CONFLICT(key) DO NOTHING",
		videoTaskStateKey(task.Provider, task.ID), payload, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save video ownership: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check video ownership write: %w", err)
	}
	if written > 0 {
		return nil
	}
	existing, ok, err := h.LoadVideoTask(ctx, task.Provider, task.ID)
	if err != nil {
		return err
	}
	if !ok || existing != task {
		return errors.New("video ownership conflicts with an existing task")
	}
	return nil
}
