// Command unipush 启动个人统一消息推送服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ziwindlu/unipush/internal/api"
	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/bark"
	"github.com/ziwindlu/unipush/internal/channel/dingtalk"
	"github.com/ziwindlu/unipush/internal/channel/email"
	"github.com/ziwindlu/unipush/internal/channel/feishu"
	"github.com/ziwindlu/unipush/internal/channel/webhook"
	"github.com/ziwindlu/unipush/internal/channel/wecom"
	"github.com/ziwindlu/unipush/internal/config"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
	"github.com/ziwindlu/unipush/internal/web"
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
	resetUser := flag.String("reset-password", "", "重置指定 Web 用户的密码后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("unipush", version)
		return nil
	}

	// 先用默认 logger 读配置，拿到 log.format / log.level 后再换成正式 logger。
	logger := newLogger(os.Stdout, "text", "info")
	slog.SetDefault(logger)

	if n, err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("加载 .env 失败", "error", err)
	} else if n > 0 {
		logger.Info("已从 .env 加载环境变量", "count", n)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger = newLogger(os.Stdout, cfg.Log.Format, cfg.Log.Level)
	slog.SetDefault(logger)
	if *resetUser != "" {
		return resetUserPassword(cfg, *resetUser, logger)
	}
	return serve(cfg, logger)
}

// resetUserPassword 为忘记密码的管理员提供离线恢复入口。
// 密码优先取 UNIPUSH_RESET_PASSWORD，未设置时随机生成并打印。
func resetUserPassword(cfg *config.Config, username string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	user, _, err := st.GetUserByUsername(ctx, strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("用户 %q 不存在", username)
		}
		return err
	}
	password := strings.TrimSpace(os.Getenv("UNIPUSH_RESET_PASSWORD"))
	generated := password == ""
	if generated {
		password, err = auth.GeneratePassword(16)
		if err != nil {
			return err
		}
	}
	if len(password) < 8 {
		return fmt.Errorf("UNIPUSH_RESET_PASSWORD 至少 8 个字符")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := st.UpdateUserPassword(ctx, user.ID, hash); err != nil {
		return err
	}
	if _, err := st.DeleteUserSessions(ctx, user.ID, ""); err != nil {
		return err
	}
	if generated {
		fmt.Printf("已重置用户 %s 的密码：%s\n", user.Username, password)
	} else {
		fmt.Printf("已重置用户 %s 的密码（来自 UNIPUSH_RESET_PASSWORD）\n", user.Username)
	}
	logger.Info("已重置 Web 用户密码", "username", user.Username, "generated", generated)
	return nil
}

// newLogger 按配置创建结构化 logger：text 适合前台人读，json 适合容器日志采集。
func newLogger(w io.Writer, format, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// serve 按已加载的配置组装依赖，并阻塞到收到退出信号。
// 配置有误（例如路由引用了不存在的渠道组）会在开始监听之前返回错误。
func serve(cfg *config.Config, logger *slog.Logger) error {
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

	if cfg.Web.Enabled {
		if _, err := web.BootstrapAdmin(ctx, st, cfg.Web.AdminUser, cfg.Web.AdminPassword, logger); err != nil {
			return fmt.Errorf("初始化 Web 控制台管理员: %w", err)
		}
	}

	dispatcher := dispatch.New(st, senders, router, dispatch.Config{
		Workers:        cfg.Dispatch.Workers,
		PollInterval:   cfg.Dispatch.PollInterval,
		Lease:          cfg.Dispatch.Lease,
		MaxAttempts:    cfg.Dispatch.MaxAttempts,
		BaseBackoff:    cfg.Dispatch.BaseBackoff,
		MaxBackoff:     cfg.Dispatch.MaxBackoff,
		RequestTimeout: cfg.Dispatch.RequestTimeout,
		Dedupe: dispatch.DedupeConfig{
			Enabled: cfg.Dedupe.Enabled,
			Window:  cfg.Dedupe.Window,
		},
		Aggregation: dispatch.AggregationConfig{
			Enabled:  cfg.Aggregation.Enabled,
			Window:   cfg.Aggregation.Window,
			MaxItems: cfg.Aggregation.MaxItems,
		},
	}, logger)

	apiServer := api.New(api.Config{
		AuthTokens:   cfg.Auth.Tokens,
		MaxBodyBytes: cfg.Server.MaxBodyBytes,
		Version:      version,
		TokenVerifier: func(ctx context.Context, token string) (bool, error) {
			_, _, err := st.VerifyAPIToken(ctx, auth.HashToken(token), time.Now().UTC())
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return true, nil
		},
	}, st, dispatcher, logger)

	handler := http.Handler(apiServer.Handler())
	if cfg.Web.Enabled {
		webServer, err := web.New(web.Config{
			Version:    version,
			SessionTTL: cfg.Web.SessionTTL,
		}, st, dispatcher, logger)
		if err != nil {
			return err
		}
		root := http.NewServeMux()
		root.Handle("/v1/", apiServer.Handler())
		root.Handle("/healthz", apiServer.Handler())
		root.Handle("/", webServer.Handler())
		handler = root
	}

	httpServer := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      handler,
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
		"log_format", cfg.Log.Format,
		"log_level", cfg.Log.Level,
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
	if cfg.Channels.Feishu.Enabled {
		s, err := feishu.New(feishu.Config{
			Webhook: cfg.Channels.Feishu.Webhook,
			Secret:  cfg.Channels.Feishu.Secret,
			MsgType: cfg.Channels.Feishu.MsgType,
			Timeout: cfg.Dispatch.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		senders[s.Name()] = s
	}
	if cfg.Channels.WeCom.Enabled {
		s, err := wecom.New(wecom.Config{
			Webhook: cfg.Channels.WeCom.Webhook,
			MsgType: cfg.Channels.WeCom.MsgType,
			Timeout: cfg.Dispatch.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		senders[s.Name()] = s
	}
	if cfg.Channels.Email.Enabled {
		s, err := email.New(email.Config{
			Host:          cfg.Channels.Email.Host,
			Port:          cfg.Channels.Email.Port,
			Username:      cfg.Channels.Email.Username,
			Password:      cfg.Channels.Email.Password,
			From:          cfg.Channels.Email.From,
			To:            cfg.Channels.Email.To,
			TLS:           cfg.Channels.Email.TLS,
			SubjectPrefix: cfg.Channels.Email.SubjectPrefix,
			Timeout:       cfg.Dispatch.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		senders[s.Name()] = s
	}
	if cfg.Channels.Webhook.Enabled {
		s, err := webhook.New(webhook.Config{
			URL:          cfg.Channels.Webhook.URL,
			Method:       cfg.Channels.Webhook.Method,
			Headers:      cfg.Channels.Webhook.Headers,
			ContentType:  cfg.Channels.Webhook.ContentType,
			BodyTemplate: cfg.Channels.Webhook.BodyTemplate,
			Timeout:      cfg.Dispatch.RequestTimeout,
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
