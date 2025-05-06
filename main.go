package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 配置结构体
type Config struct {
	StubTarName     string
	StubDirName     string
	DockerCmd       string
	TarCmd          string
	MinioAccessKey  string
	MinioSecretKey  string
	MinioContainer  string
	MinioUser       string
	MinioUserPass   string
	MinioDesc       string
	MinioAlias      string
	MinioEndpoint   string
	Timeout         time.Duration
	ConcurrentTasks int
}

// 默认配置
func DefaultConfig() *Config {
	return &Config{
		StubTarName:     "stub.tar",
		StubDirName:     "stub",
		DockerCmd:       "docker",
		TarCmd:          "tar",
		MinioAccessKey:  "yoo-oss-access-key",
		MinioSecretKey:  "yoo-oss-secret-key",
		MinioContainer:  "yoo-oss",
		MinioUser:       "minioadmin",
		MinioUserPass:   "minioadmin",
		MinioDesc:       "proxy",
		MinioAlias:      "myminio",
		MinioEndpoint:   "http://localhost:9000",
		Timeout:         5 * time.Minute,
		ConcurrentTasks: 4,
	}
}

func main() {
	// 设置上下文，添加超时控制
	ctx, cancel := context.WithTimeout(context.Background(), DefaultConfig().Timeout)
	defer cancel()

	// 初始化日志
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	if err := run(ctx, DefaultConfig()); err != nil {
		slog.Error("程序执行失败", "error", err)
		os.Exit(1)
	}

	slog.Info("初始化完成")
}

// 主要运行逻辑
func run(ctx context.Context, cfg *Config) error {
	// 获取当前工作目录
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取当前工作目录失败: %w", err)
	}

	// 检查依赖命令是否存在
	if err := checkDependencies(cfg); err != nil {
		return err
	}

	// 检查并解压主Stub文件
	stubTar := filepath.Join(cwd, cfg.StubTarName)
	if err := checkAndExtractMainStub(ctx, stubTar, cfg); err != nil {
		return err
	}

	// 处理子目录中的镜像和压缩文件
	if err := processStubDir(ctx, cwd, cfg); err != nil {
		return err
	}

	// // 启动Docker Compose
	// if err := startDockerCompose(ctx, cfg); err != nil {
	// 	return err
	// }

	// // 配置Minio
	// if err := configureMinio(ctx, cfg); err != nil {
	// 	return err
	// }

	return nil
}

// 检查必要的依赖命令
func checkDependencies(cfg *Config) error {
	dependencies := []string{cfg.TarCmd, cfg.DockerCmd}

	for _, dep := range dependencies {
		if _, err := exec.LookPath(dep); err != nil {
			return fmt.Errorf("%s 命令不存在: %w", dep, err)
		}
	}

	return nil
}

// 检查并解压主Stub文件
func checkAndExtractMainStub(ctx context.Context, stubTar string, cfg *Config) error {
	// 检查文件是否存在
	if _, err := os.Stat(stubTar); os.IsNotExist(err) {
		return fmt.Errorf("STUB 文件不存在: %w", err)
	}

	// 解压文件
	cmd := exec.CommandContext(ctx, cfg.TarCmd, "-xvf", stubTar)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("解压文件失败: %w, 输出: %s", err, output)
	}

	slog.Info("文件解压成功")
	return nil
}

// 处理Stub目录中的文件
func processStubDir(ctx context.Context, cwd string, cfg *Config) error {
	// 读取子目录
	subDirs, err := os.ReadDir(cwd)
	if err != nil {
		return fmt.Errorf("读取目录失败: %w", err)
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(subDirs))

	// 创建一个有限制的通道，用于控制并发数量
	semaphore := make(chan struct{}, cfg.ConcurrentTasks)

	for _, subDir := range subDirs {

		// 如果不是文件夹，则跳过不处理
		if !subDir.IsDir() {
			continue
		}

		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(subDir os.DirEntry) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			if err := processSubDir(ctx, filepath.Join(cwd, subDir.Name()), cfg); err != nil {
				errChan <- fmt.Errorf("处理子目录 %s 失败: %w", subDir.Name(), err)
			}
		}(subDir)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(errChan)

	// 收集所有错误
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("处理子目录时发生错误: %v", errs)
	}

	return nil
}

// 处理单个子目录
func processSubDir(ctx context.Context, subDirPath string, cfg *Config) error {
	files, err := os.ReadDir(subDirPath)
	if err != nil {
		return fmt.Errorf("读取子目录失败: %w", err)
	}

	for _, file := range files {
		filePath := filepath.Join(subDirPath, file.Name())

		// 如果文件后缀不是 .tar 则跳过不处理
		if !strings.HasSuffix(file.Name(), ".tar") {
			continue
		}

		// 处理压缩文件
		if file.Name() == "files.tar" {
			// 获取文件所在目录作为解压目标
			targetDir := filepath.Dir(filePath)
			slog.Info("正在解压文件", "file", filePath, "targetDir", targetDir)
			cmd := exec.CommandContext(ctx, cfg.TarCmd, "-xvf", filePath, "-C", targetDir)
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("tar 命令失败: %w, 输出: %s", err, output)
			}
		} else {
			slog.Info("正在加载Docker镜像", "file", filePath)
			cmd := exec.CommandContext(ctx, cfg.DockerCmd, "load", "-i", filePath)
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("docker load 命令失败: %w, 输出: %s", err, output)
			}
		}

	}

	return nil
}

// 启动Docker Compose
func startDockerCompose(ctx context.Context, cfg *Config) error {
	slog.Info("正在启动Docker Compose服务")

	// 启动docker-compose
	upCmd := exec.CommandContext(ctx, cfg.DockerCmd, "compose", "up", "-d")
	if output, err := upCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose up 命令失败: %w, 输出: %s", err, output)
	}

	// 检查docker-compose状态
	psCmd := exec.CommandContext(ctx, cfg.DockerCmd, "compose", "ps")
	output, err := psCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose ps 命令失败: %w, 输出: %s", err, output)
	}

	slog.Info("Docker Compose服务已启动", "status", string(output))
	return nil
}

// 配置Minio
func configureMinio(ctx context.Context, cfg *Config) error {
	slog.Info("正在配置Minio")

	// 等待Minio服务启动
	time.Sleep(5 * time.Second)

	// 配置Minio别名
	aliasCmd := exec.CommandContext(
		ctx,
		cfg.DockerCmd,
		"exec",
		cfg.MinioContainer,
		"mc",
		"alias",
		"set",
		cfg.MinioAlias,
		cfg.MinioEndpoint,
		cfg.MinioUser,
		cfg.MinioUserPass,
	)

	if output, err := aliasCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("minio alias 命令失败: %w, 输出: %s", err, output)
	}

	// 创建Minio访问密钥
	accessKeyCmd := exec.CommandContext(
		ctx,
		cfg.DockerCmd,
		"exec",
		cfg.MinioContainer,
		"mc",
		"admin",
		"accesskey",
		"create",
		cfg.MinioAlias,
		cfg.MinioUser,
		fmt.Sprintf("--access-key=%s", cfg.MinioAccessKey),
		fmt.Sprintf("--secret-key=%s", cfg.MinioSecretKey),
		"--name",
		cfg.MinioDesc,
		"--description",
		cfg.MinioDesc,
	)

	if output, err := accessKeyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("minio accesskey 命令失败: %w, 输出: %s", err, output)
	}

	slog.Info("Minio配置完成")
	return nil
}
