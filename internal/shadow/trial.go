// Package shadow connects the authoritative account engine to its silent ledger replica.
package shadow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sergeant/internal/database"
	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/ledger"
	"sergeant/internal/ledger/store"
	"sergeant/internal/legacy"
)

type Trial struct {
	Legacy             *legacy.Engine
	id, path, currency string
	translator         *i18n.Translator
}

func Open(ctx context.Context, primaryPath, ghostPath, currency string, tr *i18n.Translator) (*Trial, error) {
	for _, path := range []string{primaryPath, ghostPath} {
		if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
			return nil, errors.New("the shadow trial requires two SQLite filesystem paths")
		}
	}
	primaryPath, err := filepath.Abs(primaryPath)
	if err != nil {
		return nil, err
	}
	ghostPath, err = filepath.Abs(ghostPath)
	if err != nil {
		return nil, err
	}
	a, _ := os.Stat(primaryPath)
	b, _ := os.Stat(ghostPath)
	if primaryPath == ghostPath || (a != nil && b != nil && os.SameFile(a, b)) {
		return nil, errors.New("legacy and ledger paths refer to the same database")
	}
	primary, err := legacy.Open(primaryPath, currency, tr)
	if err != nil {
		return nil, err
	}
	id, err := primary.PrepareGhost(ctx, ghostPath)
	if err != nil {
		_ = primary.Close()
		return nil, fmt.Errorf("initialize ledger snapshot: %w", err)
	}
	return &Trial{Legacy: primary, id: id, path: ghostPath, currency: currency, translator: tr}, nil
}

func (t *Trial) openLedger(ctx context.Context) (*store.SQLite, error) {
	if err := database.CheckGhost(ctx, t.path, t.id); err != nil {
		return nil, err
	}
	return store.Open(database.FileURI(t.path, "rw"))
}

type replayer interface {
	Replay(context.Context, engine.Delivery) error
}

// A panic in experimental ledger code must leave the command available for
// replay and the live Slack process running. Panic values can contain input.
func replay(ctx context.Context, ghost replayer, d engine.Delivery) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("ledger panic (%T)", failure)
		}
	}()
	return ghost.Replay(ctx, d)
}

func (t *Trial) drain(ctx context.Context, ghost replayer) error {
	for ctx.Err() == nil {
		d, ok, err := t.Legacy.Next(ctx)
		if err != nil || !ok {
			return err
		}
		if err = replay(ctx, ghost, d); err != nil {
			return fmt.Errorf("replay command %d: %w", d.Sequence, err)
		}
		if err = t.Legacy.Acknowledge(ctx, d.Sequence); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Run retries ghost failures independently of the Slack request lifetime.
func (t *Trial) Run(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var db *store.SQLite
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	var lastError string
	for ctx.Err() == nil {
		var err error
		if db == nil {
			db, err = t.openLedger(ctx)
		}
		if err == nil {
			err = t.drain(ctx, ledger.New(db, t.currency, t.translator))
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if err.Error() != lastError {
				log.Error("ledger replay paused", "err", err)
				lastError = err.Error()
			}
			if db != nil {
				_ = db.Close()
				db = nil
			}
		} else if lastError != "" {
			log.Info("ledger replay caught up")
			lastError = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
