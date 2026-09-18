// tgdl: a local Telegram media downloader with a browser UI. Downloads run on tdl's core engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/gaoze24/tg-batch-dl/internal/config"
	"github.com/gaoze24/tg-batch-dl/internal/jobs"
	"github.com/gaoze24/tg-batch-dl/internal/sysutil"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
	"github.com/gaoze24/tg-batch-dl/internal/web"
)

var version = "dev" // set by -ldflags "-X main.version=..."

const maxLogSize = 5 << 20

func main() {
	dataFlag := flag.String("data", "", "数据目录（登录信息、配置、日志）；默认是程序旁边的 tgdl-data 文件夹")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	flag.Parse()

	if err := run(*dataFlag, !*noBrowser); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		fmt.Fprintln(os.Stderr, "按回车键退出…")
		_, _ = fmt.Scanln()
		os.Exit(1)
	}
}

func run(dataFlag string, openBrowser bool) error {
	dataDir, err := resolveDataDir(dataFlag)
	if err != nil {
		return err
	}
	store, err := config.Open(dataDir)
	if err != nil {
		return err
	}
	settings := store.Get()
	url := fmt.Sprintf("http://127.0.0.1:%d/", settings.Port)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", settings.Port))
	if err != nil {
		// most likely another copy is already running: just show it
		fmt.Println("端口已被占用，可能程序已经在运行，正在打开页面：", url)
		_ = sysutil.OpenURL(url)
		time.Sleep(2 * time.Second)
		return nil
	}

	log, closeLog, err := newLogger(filepath.Join(dataDir, "tgdl.log"))
	if err != nil {
		return err
	}
	defer closeLog()
	log.Info("start", zap.String("version", version), zap.String("data", dataDir))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tgClient := tgc.New(filepath.Join(dataDir, "session.json"), settings.Proxy, settings.APIID, settings.APIHash, log.Named("tg"))
	manager := jobs.NewManager(tgClient, store.Get, log.Named("jobs"))
	srv := &http.Server{
		Handler: (&web.Server{
			Version: version,
			Port:    settings.Port,
			TG:      tgClient,
			Jobs:    manager,
			Config:  store,
			Quit:    stop,
			Log:     log.Named("web"),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go tgClient.Run(ctx)
	go manager.Run(ctx)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", zap.Error(err))
			stop()
		}
	}()

	fmt.Printf("tgdl %s 已启动\n", version)
	fmt.Println("  页面地址：", url)
	fmt.Println("  数据目录：", dataDir, "（里面的 session.json 就是你的登录凭证，不要发给别人）")
	fmt.Println("  关闭这个窗口即退出程序")
	if openBrowser {
		_ = sysutil.OpenURL(url)
	}

	<-ctx.Done()
	fmt.Println("正在退出…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// resolveDataDir prefers a portable "tgdl-data" folder next to the executable, falling back to the user config dir
// when that location isn't writable (e.g. Program Files).
func resolveDataDir(flagValue string) (string, error) {
	if flagValue != "" {
		dir, err := filepath.Abs(flagValue)
		if err != nil {
			return "", err
		}
		return dir, os.MkdirAll(dir, 0o700)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "tgdl-data")
		if writable(dir) {
			return dir, nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "tgdl")
	return dir, os.MkdirAll(dir, 0o700)
}

func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// newLogger writes JSON logs to path, starting fresh once the file grows past maxLogSize.
func newLogger(path string) (*zap.Logger, func(), error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(f), zapcore.InfoLevel)
	log := zap.New(core)
	return log, func() { _ = log.Sync(); _ = f.Close() }, nil
}
