// Command unipush 启动个人统一消息推送服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ziwindlu/unipush/internal/api"
	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/bark"
	"github.com/ziwindlu/unipush/internal/channel/dingtalk"
	"github.com/ziwindlu/unipush/internal/config"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("unipush 启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	defaultConfig := "config.yaml"
	if v := os.Getenv("UNIPUSH_CONFIG"); v != "" {
		defaultConfig = v
	}
	configPath := flag.String("config", defaultConfig, "配置文件路径")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("unipush", version)
		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	return serve(*configPath, logger)
}

// serve 加载配置、组装依赖并阻塞到收到退出信号。
// 配置有误（例如路由引用了不存在的渠道组）会在开始监听之前返回错误。
func serve(configPath string, logger *slog.Logger) error {
	if n, err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("加载 .env 失败", "error", err)
	} else if n > 0 {
		logger.Info("已从 .env 加载环境变量", "count", n)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}
	if cfg.EphemeralToken {
		logger.Warn("本次运行的临时鉴权 token（仅本机调试用）", "token", cfg.Auth.Tokens[0])
	}

	senders, err := buildSenders(cfg, logger)
	if err != nil {
		return err
	}

	router, err := buildRouter(cfg, senders)
	if err != nil {
		return err
	}
	for _, warning := range router.Warnings() {
		logger.Warn(warning)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	dispatcher := dispatch.New(st, senders, router, dispatch.Config{
		Workers:        cfg.Dispatch.Workers,
		PollInterval:   cfg.Dispatch.PollInterval,
		Lease:          cfg.Dispatch.Lease,
		MaxAttempts:    cfg.Dispatch.MaxAttempts,
		BaseBackoff:    cfg.Dispatch.BaseBackoff,
		MaxBackoff:     cfg.Dispatch.MaxBackoff,
		RequestTimeout: cfg.Dispatch.RequestTimeout,
	}, logger)

	apiServer := api.New(api.Config{
		AuthTokens:   cfg.Auth.Tokens,
		MaxBodyBytes: cfg.Server.MaxBodyBytes,
	}, st, dispatcher, logger)

	httpServer := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      apiServer.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// 投递 worker 有自己的生命周期：HTTP 停止接客后先让它把手上的活做完再退出。
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatcher.Run(dispatchCtx)
	}()

	serverErr := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()

	logger.Info("unipush 已启动",
		"version", version,
		"addr", cfg.Server.Addr,
		"database", cfg.Database.Path,
		"channels", dispatcher.Channels(),
		"groups", dispatcher.Groups(),
	)

	select {
	case err := <-serverErr:
		stopDispatch()
		<-dispatchDone
		if err != nil {
			return fmt.Errorf("HTTP 服务异常: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅退出")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)

	stopDispatch()
	<-dispatchDone

	if shutdownErr != nil {
		return fmt.Errorf("优雅退出超时: %w", shutdownErr)
	}
	logger.Info("unipush 已退出")
	return nil
}

// buildSenders 按配置创建已启用的渠道适配器。
func buildSenders(cfg *config.Config, logger *slog.Logger) (channel.Set, error) {
	senders := channel.Set{}
	if cfg.Channels.Bark.Enabled {
		s, err := bark.New(bark.Config{
			ServerURL: cfg.Channels.Bark.ServerURL,
			DeviceKey: cfg.Channels.Bark.DeviceKey,
			Group:     cfg.Channels.Bark.Group,
			Sound:     cfg.Channels.Bark.Sound,
			Timeout:   cfg.Dispatch.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		senders[s.Name()] = s
	}
	if cfg.Channels.DingTalk.Enabled {
		s, err := dingtalk.New(dingtalk.Config{
			Webhook: cfg.Channels.DingTalk.Webhook,
			Secret:  cfg.Channels.DingTalk.Secret,
			Keyword: cfg.Channels.DingTalk.Keyword,
			Timeout: cfg.Dispatch.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		senders[s.Name()] = s
	}
	if len(senders) == 0 {
		logger.Warn("没有启用任何渠道，消息入口会返回 503，直到配置至少一个渠道")
	}
	return senders, nil
}

// buildRouter 校验并编译路由配置。引用不存在的渠道组 / 渠道会让启动直接失败。
func buildRouter(cfg *config.Config, senders channel.Set) (*routing.Router, error) {
	return routing.New(cfg.Routing, routing.Options{
		SupportedChannels: channel.Known(),
		EnabledChannels:   senders.Names(),
	})
}
