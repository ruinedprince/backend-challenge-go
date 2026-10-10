package app

import (
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/fx"
)

// Application owns the Fx runtime. Keeping Fx here prevents framework types
// from leaking into the domain packages we will add in the next phase.
type Application struct {
	fxApp *fx.App
}

func New(logger *slog.Logger) (*Application, error) {
	fxApp := fx.New(
		fx.Supply(logger),
		fx.StartTimeout(20*time.Second),
		fx.StopTimeout(15*time.Second),
		Module,
	)

	if err := fxApp.Err(); err != nil {
		return nil, fmt.Errorf("build fx application: %w", err)
	}

	return &Application{fxApp: fxApp}, nil
}

func (a *Application) Run() {
	a.fxApp.Run()
}
