package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/smonte/sisu/internal/cache"
	"github.com/smonte/sisu/internal/fs"
	"github.com/spf13/cobra"
)

var (
	profile    string
	region     string
	background bool
	mountpoint string
	debug      bool
)

func defaultMountpoint() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/sisu"
	}
	return filepath.Join(home, ".sisu", "mnt")
}

var rootCmd = &cobra.Command{
	Use:   "sisu",
	Short: "Browse AWS resources as a filesystem",
	Long: `sisu mounts your AWS resources (S3, DynamoDB, etc.) as a local filesystem.

Simply run 'sisu' to mount and open an interactive shell.
Use standard commands like ls, cd, cat to browse your resources.
Type 'exit' to unmount and return to your original shell.`,
	RunE: runSisu,
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Unmount a background sisu mount",
	RunE:  runStop,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "AWS profile to use")
	rootCmd.PersistentFlags().StringVar(&region, "region", "", "AWS region to use")
	rootCmd.PersistentFlags().BoolVar(&background, "background", false, "Run in background (daemon mode)")
	rootCmd.PersistentFlags().StringVar(&mountpoint, "mountpoint", "", "Custom mount point (default: ~/.sisu/mnt)")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "Enable debug logging")

	rootCmd.AddCommand(stopCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runSisu(cmd *cobra.Command, args []string) error {
	mp := mountpoint
	if mp == "" {
		mp = defaultMountpoint()
	}

	// Create mountpoint if it doesn't exist
	if err := os.MkdirAll(mp, 0755); err != nil {
		return fmt.Errorf("failed to create mountpoint: %w", err)
	}

	// Check if already mounted
	if isMounted(mp) {
		return fmt.Errorf("already mounted at %s, run 'sisu stop' first", mp)
	}

	fmt.Println("Mounting AWS resources to", mp+"...")
	if profile != "" {
		fmt.Println("Using profile:", profile)
	}
	if region != "" {
		fmt.Println("Region:", region)
	}
	if debug {
		fmt.Println("Debug mode: enabled")
		cache.Debug = true
		fs.Debug = true
	}

	// Create and mount the filesystem
	sisuFS, err := fs.NewSisuFS(fs.Config{
		Profile: profile,
		Region:  region,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}

	server, err := sisuFS.Mount(mp)
	if err != nil {
		return fmt.Errorf("failed to mount: %w", err)
	}

	if background {
		fmt.Println("\nMounted! Run 'sisu stop' to unmount.")
		fmt.Println("Mount point:", mp)

		// Write PID file for stop command
		pidFile := filepath.Join(filepath.Dir(mp), "sisu.pid")
		if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			server.Unmount()
			return fmt.Errorf("failed to write pid file: %w", err)
		}

		// Wait for signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		fmt.Println("\nUnmounting...")
		server.Unmount()
		os.Remove(pidFile)
	} else {
		fmt.Println("\nMounted! Opening new shell. Type 'exit' to unmount.")
		fmt.Println()

		// Spawn a new shell
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}

		// Determine shell type for prompt customization
		var shellCmd *exec.Cmd
		if strings.Contains(shell, "zsh") {
			// For zsh, use ZDOTDIR trick or pass prompt via -c
			shellCmd = exec.Command(shell, "-c", fmt.Sprintf(`
				PROMPT='sisu:%%~ $ '
				cd %q
				exec %s -i
			`, mp, shell))
		} else {
			// For bash/sh, use --rcfile or PS1
			shellCmd = exec.Command(shell, "--rcfile", "/dev/null", "-i")
			shellCmd.Dir = mp
			shellCmd.Env = append(os.Environ(),
				"SISU_MOUNT="+mp,
				`PS1=sisu:\w $ `,
			)
		}

		shellCmd.Stdin = os.Stdin
		shellCmd.Stdout = os.Stdout
		shellCmd.Stderr = os.Stderr

		if err := shellCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Shell error: %v\n", err)
		}

		fmt.Println("\nUnmounting...")
		server.Unmount()
		fmt.Println("Done.")
	}

	return nil
}

func runStop(cmd *cobra.Command, args []string) error {
	mp := mountpoint
	if mp == "" {
		mp = defaultMountpoint()
	}

	pidFile := filepath.Join(filepath.Dir(mp), "sisu.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		// Try to unmount directly
		if isMounted(mp) {
			return unmountDirect(mp)
		}
		return fmt.Errorf("no sisu mount found at %s", mp)
	}

	var pid int
	fmt.Sscanf(string(pidBytes), "%d", &pid)

	// Send SIGTERM to the sisu process
	proc, err := os.FindProcess(pid)
	if err != nil {
		return unmountDirect(mp)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return unmountDirect(mp)
	}

	fmt.Println("Unmount signal sent.")
	return nil
}

func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), filepath.Clean(path))
}

func unmountDirect(path string) error {
	cmd := exec.Command("fusermount", "-u", path)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to unmount: %w", err)
	}
	fmt.Println("Unmounted", path)
	return nil
}
