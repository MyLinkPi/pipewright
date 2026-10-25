// restore_cli.go:CLI 离线恢复子命令(pipewright restore <archive.pwb>)。
// 在 main 打开存储之前分派(与 tunnel-helper 同一先例):进程此时不持有数据库
// 句柄,可安全替换 SQLite 文件 / 回放 MySQL dump。
//
// 用法:
//
//	pipewright restore <archive.pwb> [--passphrase-file <file>]
//
// 口令亦可经环境变量 PIPEWRIGHT_RESTORE_PASSPHRASE 提供(命令行参数会进 ps,
// 不提供 --passphrase 直传)。恢复前自动把现库转存 restore-safety/。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/huangchengsir/pipewright/internal/backup"
	"github.com/huangchengsir/pipewright/internal/config"
)

// runRestoreCLI 执行离线恢复,返回进程退出码。
func runRestoreCLI(cfg config.Config, args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	passFile := fs.String("passphrase-file", "", "包含恢复口令的文件(归档带 master key 时需要)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "用法: pipewright restore <archive.pwb> [--passphrase-file <file>]")
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "用法: pipewright restore <archive.pwb> [--passphrase-file <file>]")
		return 2
	}
	archive, err := filepath.Abs(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位归档失败: %v\n", err)
		return 1
	}

	passphrase := strings.TrimSpace(os.Getenv("PIPEWRIGHT_RESTORE_PASSPHRASE"))
	if passphrase == "" && *passFile != "" {
		raw, err := os.ReadFile(*passFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取口令文件失败: %v\n", err)
			return 1
		}
		passphrase = strings.TrimSpace(string(raw))
	}

	driver, dsn, err := cfg.StoreConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置无效: %v\n", err)
		return 1
	}
	opt := backup.RestoreOptions{Driver: driver, Passphrase: passphrase}
	if driver == "mysql" {
		opt.MySQLDSN = dsn
		if wd, werr := os.Getwd(); werr == nil {
			opt.DataDir = wd
		} else {
			opt.DataDir = "."
		}
	} else {
		if abs, aerr := filepath.Abs(dsn); aerr == nil {
			opt.DBPath, opt.DataDir = abs, filepath.Dir(abs)
		} else {
			opt.DBPath, opt.DataDir = dsn, filepath.Dir(dsn)
		}
	}

	fmt.Printf("校验归档: %s\n", archive)
	if _, err := backup.ValidateArchive(archive); err != nil {
		fmt.Fprintf(os.Stderr, "归档校验失败: %v\n", err)
		return 1
	}
	fmt.Println("应用恢复(恢复前会把现数据转存到 restore-safety/)…")
	rep, err := backup.ApplyRestore(archive, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "恢复失败: %v\n(现库已转存 restore-safety/,如需回退把它复制回原位即可)\n", err)
		return 1
	}

	fmt.Println("恢复完成:")
	if rep.AppliedDB {
		fmt.Printf("  - 数据库已恢复(此前数据备份在 %s)\n", rep.DBSafetyPath)
	}
	if rep.AppliedAcme {
		fmt.Println("  - acme/ 证书账号目录已恢复")
	}
	if rep.AppliedArtifacts {
		fmt.Println("  - artifacts/ 制品库已恢复")
	}
	switch rep.MasterKeyAction {
	case "written":
		fmt.Printf("  - master key 已写入 %s\n", rep.MasterKeyFilePath)
		fmt.Println("    请将其配置为 PIPEWRIGHT_MASTER_KEY(或 PIPEWRIGHT_MASTER_KEY_FILE 指向该文件)后再启动,并妥善保管/删除该文件")
	case "present":
		fmt.Println("  - 警告:归档携带 master key 但未提供口令,未恢复;请配置原 PIPEWRIGHT_MASTER_KEY 或带口令重试")
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  - 注意: %s\n", w)
	}
	fmt.Println("现在可以启动 pipewright 服务了。")
	return 0
}

// backupRestoreOptions 由 driver+dsn 推导恢复/备份的数据落点(mysql 模式数据目录
// = 工作目录,与 admin_password.txt 的落盘惯例一致)。
func backupRestoreOptions(driver, dsn string) backup.RestoreOptions {
	opt := backup.RestoreOptions{Driver: driver}
	if driver == "mysql" {
		opt.MySQLDSN = dsn
		if wd, werr := os.Getwd(); werr == nil {
			opt.DataDir = wd
		} else {
			opt.DataDir = "."
		}
		return opt
	}
	if abs, aerr := filepath.Abs(dsn); aerr == nil {
		opt.DBPath, opt.DataDir = abs, filepath.Dir(abs)
	} else {
		opt.DBPath, opt.DataDir = dsn, filepath.Dir(dsn)
	}
	return opt
}
