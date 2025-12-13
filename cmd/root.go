package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/semonte/sisu/internal/cache"
	"github.com/semonte/sisu/internal/fs"
	"github.com/semonte/sisu/internal/provider"
	"github.com/spf13/cobra"
)

var (
	profile    string
	region     string
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
	Short: "Unmount sisu",
	RunE:  runStop,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "AWS profile to use")
	rootCmd.PersistentFlags().StringVar(&region, "region", "", "AWS region to use")
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
		provider.Debug = true
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

	shellCmd.Run() // ignore exit status - it's just the shell's last command status

	fmt.Println("\nUnmounting...")
	server.Unmount()
	fmt.Println("Done.")

	return nil
}

func runStop(cmd *cobra.Command, args []string) error {
	mp := mountpoint
	if mp == "" {
		mp = defaultMountpoint()
	}

	if !isMounted(mp) {
		return fmt.Errorf("no sisu mount found at %s", mp)
	}

	return unmountDirect(mp)
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
