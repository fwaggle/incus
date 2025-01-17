package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/canonical/lxd/client"
	lxdAPI "github.com/canonical/lxd/shared/api"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus/client"
	cli "github.com/lxc/incus/internal/cmd"
	"github.com/lxc/incus/internal/linux"
	"github.com/lxc/incus/internal/version"
	incusAPI "github.com/lxc/incus/shared/api"
	"github.com/lxc/incus/shared/subprocess"
	"github.com/lxc/incus/shared/util"
)

type cmdGlobal struct {
	asker cli.Asker

	flagHelp    bool
	flagVersion bool
}

func main() {
	// Setup command line parser.
	migrateCmd := cmdMigrate{}

	app := migrateCmd.Command()
	app.Use = "lxd-to-incus"
	app.Short = "LXD to Incus migration tool"
	app.Long = `Description:
  LXD to Incus migration tool

  This tool allows an existing LXD user to move all their data over to Incus.
`
	app.SilenceUsage = true
	app.CompletionOptions = cobra.CompletionOptions{DisableDefaultCmd: true}

	// Global flags.
	globalCmd := cmdGlobal{asker: cli.NewAsker(bufio.NewReader(os.Stdin))}
	migrateCmd.global = globalCmd
	app.PersistentFlags().BoolVar(&globalCmd.flagVersion, "version", false, "Print version number")
	app.PersistentFlags().BoolVarP(&globalCmd.flagHelp, "help", "h", false, "Print help")

	// Version handling.
	app.SetVersionTemplate("{{.Version}}\n")
	app.Version = version.Version

	// Run the main command and handle errors.
	err := app.Execute()
	if err != nil {
		os.Exit(1)
	}
}

type cmdMigrate struct {
	global cmdGlobal

	flagYes           bool
	flagClusterMember bool
}

func (c *cmdMigrate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "lxd-to-incus"
	cmd.RunE = c.Run
	cmd.PersistentFlags().BoolVar(&c.flagYes, "yes", false, "Migrate without prompting")
	cmd.PersistentFlags().BoolVar(&c.flagClusterMember, "cluster-member", false, "Used internally for cluster migrations")

	return cmd
}

func (c *cmdMigrate) Run(app *cobra.Command, args []string) error {
	var err error
	var srcClient lxd.InstanceServer
	var targetClient incus.InstanceServer

	// Confirm that we're root.
	if os.Geteuid() != 0 {
		return fmt.Errorf("This tool must be run as root")
	}

	// Iterate through potential sources.
	fmt.Println("=> Looking for source server")
	var source Source
	for _, candidate := range sources {
		if !candidate.Present() {
			continue
		}

		source = candidate
		break
	}

	if source == nil {
		return fmt.Errorf("No source server could be found")
	}

	fmt.Printf("==> Detected: %s\n", source.Name())

	// Iterate through potential targets.
	fmt.Println("=> Looking for target server")
	var target Target
	for _, candidate := range targets {
		if !candidate.Present() {
			continue
		}

		target = candidate
		break
	}

	if target == nil {
		return fmt.Errorf("No target server could be found")
	}

	// Connect to the servers.
	clustered := c.flagClusterMember
	if !c.flagClusterMember {
		fmt.Println("=> Connecting to source server")
		srcClient, err = source.Connect()
		if err != nil {
			return fmt.Errorf("Failed to connect to the source: %w", err)
		}

		srcServerInfo, _, err := srcClient.GetServer()
		if err != nil {
			return fmt.Errorf("Failed to get source server info: %w", err)
		}

		clustered = srcServerInfo.Environment.ServerClustered
	}

	fmt.Println("=> Connecting to the target server")
	targetClient, err = target.Connect()
	if err != nil {
		return fmt.Errorf("Failed to connect to the target: %w", err)
	}

	// Configuration validation.
	if !c.flagClusterMember {
		err = c.validate(source, target)
		if err != nil {
			return err
		}
	}

	// Grab the path information.
	sourcePaths, err := source.Paths()
	if err != nil {
		return fmt.Errorf("Failed to get source paths: %w", err)
	}

	targetPaths, err := target.Paths()
	if err != nil {
		return fmt.Errorf("Failed to get target paths: %w", err)
	}

	// Mangle storage pool sources.
	rewriteStatements := []string{}
	rewriteCommands := [][]string{}

	if !c.flagClusterMember {
		var storagePools []lxdAPI.StoragePool
		if !clustered {
			storagePools, err = srcClient.GetStoragePools()
			if err != nil {
				return fmt.Errorf("Couldn't list storage pools: %w", err)
			}
		} else {
			clusterMembers, err := srcClient.GetClusterMembers()
			if err != nil {
				return fmt.Errorf("Failed to retrieve the list of cluster members")
			}

			for _, member := range clusterMembers {
				poolNames, err := srcClient.UseTarget(member.ServerName).GetStoragePoolNames()
				if err != nil {
					return fmt.Errorf("Couldn't list storage pools: %w", err)
				}

				for _, poolName := range poolNames {
					pool, _, err := srcClient.UseTarget(member.ServerName).GetStoragePool(poolName)
					if err != nil {
						return fmt.Errorf("Couldn't get storage pool: %w", err)
					}

					storagePools = append(storagePools, *pool)
				}
			}
		}

		rbdRenamed := []string{}
		for _, pool := range storagePools {
			if pool.Driver == "ceph" {
				cluster, ok := pool.Config["ceph.cluster_name"]
				if !ok {
					cluster = "ceph"
				}

				client, ok := pool.Config["ceph.user.name"]
				if !ok {
					client = "admin"
				}

				rbdPool, ok := pool.Config["ceph.osd.pool_name"]
				if !ok {
					rbdPool = pool.Name
				}

				renameCmd := []string{"rbd", "rename", "--cluster", cluster, "--name", client, fmt.Sprintf("%s/lxd_%s", rbdPool, rbdPool), fmt.Sprintf("%s/incus_%s", rbdPool, rbdPool)}
				if !util.ValueInSlice(pool.Name, rbdRenamed) {
					rewriteCommands = append(rewriteCommands, renameCmd)
					rbdRenamed = append(rbdRenamed, pool.Name)
				}
			}

			source := pool.Config["source"]
			if source == "" || source[0] != byte('/') {
				continue
			}

			if !strings.HasPrefix(source, sourcePaths.Daemon) {
				continue
			}

			newSource := strings.Replace(source, sourcePaths.Daemon, targetPaths.Daemon, 1)
			rewriteStatements = append(rewriteStatements, fmt.Sprintf("UPDATE storage_pools_config SET value='%s' WHERE value='%s';", newSource, source))
		}
	}

	// Mangle OVS/OVN.
	srcServerInfo, _, err := srcClient.GetServer()
	if err != nil {
		return fmt.Errorf("Failed to get source server info: %w", err)
	}

	ovnNB, ok := srcServerInfo.Config["network.ovn.northbound_connection"].(string)
	if ok && ovnNB != "" {
		if !c.flagClusterMember {
			out, err := subprocess.RunCommand("ovs-vsctl", "get", "open_vswitch", ".", "external_ids:ovn-remote")
			if err != nil {
				return fmt.Errorf("Failed to get OVN southbound database address: %w", err)
			}

			ovnSB := strings.TrimSpace(strings.Replace(out, "\"", "", -1))

			commands, err := ovnConvert(ovnNB, ovnSB)
			if err != nil {
				return fmt.Errorf("Failed to prepare OVN conversion: %v", err)
			}

			rewriteCommands = append(rewriteCommands, commands...)
		}

		commands, err := ovsConvert()
		if err != nil {
			return fmt.Errorf("Failed to prepare OVS conversion: %v", err)
		}

		rewriteCommands = append(rewriteCommands, commands...)
	}

	// Confirm migration.
	if !c.flagClusterMember && !c.flagYes {
		if !clustered {
			fmt.Println(`
The migration is now ready to proceed.
At this point, the source server and all its instances will be stopped.
Instances will come back online once the migration is complete.
`)

			ok, err := c.global.asker.AskBool("Proceed with the migration? [default=no]: ", "no")
			if err != nil {
				return err
			}

			if !ok {
				os.Exit(1)
			}
		} else {
			fmt.Println(`
The migration is now ready to proceed.

A cluster environment was detected.
Manual action will be needed on each of the server prior to Incus being functional.`)

			if os.Getenv("CLUSTER_NO_STOP") != "1" {
				fmt.Println("The migration will begin by shutting down instances on all servers.")
			}

			fmt.Println(`
It will then convert the current server over to Incus and then wait for the other servers to be converted.

Do not attempt to manually run this tool on any of the other servers in the cluster.
Instead this tool will be providing specific commands for each of the servers.
`)

			ok, err := c.global.asker.AskBool("Proceed with the migration? [default=no]: ", "no")
			if err != nil {
				return err
			}

			if !ok {
				os.Exit(1)
			}
		}
	}

	// Cluster evacuation.
	if !c.flagClusterMember && clustered && os.Getenv("CLUSTER_NO_EVACUTE") != "1" {
		fmt.Println("=> Stopping all workloads on the cluster")

		clusterMembers, err := srcClient.GetClusterMembers()
		if err != nil {
			return fmt.Errorf("Failed to retrieve the list of cluster members")
		}

		for _, member := range clusterMembers {
			fmt.Printf("==> Stopping all workloads on server %q\n", member.ServerName)

			op, err := srcClient.UpdateClusterMemberState(member.ServerName, lxdAPI.ClusterMemberStatePost{Action: "evacuate", Mode: "stop"})
			if err != nil {
				return fmt.Errorf("Failed to stop workloads %q: %w", member.ServerName, err)
			}

			err = op.Wait()
			if err != nil {
				return fmt.Errorf("Failed to stop workloads %q: %w", member.ServerName, err)
			}
		}
	}

	// Stop source.
	fmt.Println("=> Stopping the source server")
	err = source.Stop()
	if err != nil {
		fmt.Errorf("Failed to stop the source server: %w", err)
	}

	// Stop target.
	fmt.Println("=> Stopping the target server")
	err = target.Stop()
	if err != nil {
		fmt.Errorf("Failed to stop the target server: %w", err)
	}

	// Unmount potential mount points.
	for _, mount := range []string{"guestapi", "shmounts"} {
		_ = unix.Unmount(filepath.Join(targetPaths.Daemon, mount), unix.MNT_DETACH)
	}

	// Wipe the target.
	fmt.Println("=> Wiping the target server")

	err = os.RemoveAll(targetPaths.Logs)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Failed to remove %q: %w", targetPaths.Logs, err)
	}

	err = os.RemoveAll(targetPaths.Cache)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Failed to remove %q: %w", targetPaths.Cache, err)
	}

	err = os.RemoveAll(targetPaths.Daemon)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Failed to remove %q: %w", targetPaths.Daemon, err)
	}

	// Migrate data.
	fmt.Println("=> Migrating the data")

	_, err = subprocess.RunCommand("mv", sourcePaths.Logs, targetPaths.Logs)
	if err != nil {
		return fmt.Errorf("Failed to move %q to %q: %w", sourcePaths.Logs, targetPaths.Logs, err)
	}

	_, err = subprocess.RunCommand("mv", sourcePaths.Cache, targetPaths.Cache)
	if err != nil {
		return fmt.Errorf("Failed to move %q to %q: %w", sourcePaths.Cache, targetPaths.Cache, err)
	}

	if linux.IsMountPoint(sourcePaths.Daemon) {
		err = os.MkdirAll(targetPaths.Daemon, 0711)
		if err != nil {
			return fmt.Errorf("Failed to create target directory: %w", err)
		}

		err = unix.Mount(sourcePaths.Daemon, targetPaths.Daemon, "none", unix.MS_BIND|unix.MS_REC, "")
		if err != nil {
			return fmt.Errorf("Failed to bind mount %q to %q: %w", sourcePaths.Daemon, targetPaths.Daemon, err)
		}

		err = unix.Unmount(sourcePaths.Daemon, unix.MNT_DETACH)
		if err != nil {
			return fmt.Errorf("Failed to unmount source mount %q: %w", sourcePaths.Daemon, err)
		}

		fmt.Println("")
		fmt.Printf("WARNING: %s was detected to be a mountpoint.\n", sourcePaths.Daemon)
		fmt.Printf("The migration logic has moved this mount to the new target path at %s.\n", targetPaths.Daemon)
		fmt.Printf("However it is your responsability to modify your system settings to ensure this mount will be properly restored on reboot.\n")
		fmt.Println("")
	} else {
		_, err = subprocess.RunCommand("mv", sourcePaths.Daemon, targetPaths.Daemon)
		if err != nil {
			return fmt.Errorf("Failed to move %q to %q: %w", sourcePaths.Daemon, targetPaths.Daemon, err)
		}
	}

	// Migrate database format.
	fmt.Println("=> Migrating database")
	err = migrateDatabase(filepath.Join(targetPaths.Daemon, "database"))
	if err != nil {
		return fmt.Errorf("Failed to migrate database in %q: %w", filepath.Join(targetPaths.Daemon, "database"), err)
	}

	// Apply custom migration statements.
	if len(rewriteStatements) > 0 {
		fmt.Println("=> Writing database patch")
		err = os.WriteFile(filepath.Join(targetPaths.Daemon, "database", "patch.global.sql"), []byte(strings.Join(rewriteStatements, "\n")+"\n"), 0600)
		if err != nil {
			return fmt.Errorf("Failed to write database path: %w", err)
		}
	}

	if len(rewriteCommands) > 0 {
		fmt.Println("=> Running data migration commands")
		for _, cmd := range rewriteCommands {
			_, err := subprocess.RunCommand(cmd[0], cmd[1:]...)
			if err != nil {
				return err
			}
		}
	}

	// Cleanup paths.
	fmt.Println("=> Cleaning up target paths")

	for _, dir := range []string{"backups", "images"} {
		// Remove any potential symlink (ignore errors for real directories).
		_ = os.Remove(filepath.Join(targetPaths.Daemon, dir))
	}

	for _, dir := range []string{"devices", "devlxd", "security", "shmounts"} {
		err = os.RemoveAll(filepath.Join(targetPaths.Daemon, dir))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("Failed to delete %q: %w", dir, err)
		}
	}

	for _, dir := range []string{"containers", "containers-snapshots", "snapshots", "virtual-machines", "virtual-machines-snapshots"} {
		entries, err := os.ReadDir(filepath.Join(targetPaths.Daemon, dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("Failed to read entries in %q: %w", filepath.Join(targetPaths.Daemon, dir), err)
		}

		for _, entry := range entries {
			srcPath := filepath.Join(targetPaths.Daemon, dir, entry.Name())

			if entry.Type()&os.ModeSymlink != os.ModeSymlink {
				continue
			}

			oldTarget, err := os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("Failed to resolve symlink %q: %w", srcPath, err)
			}

			newTarget := strings.Replace(oldTarget, sourcePaths.Daemon, targetPaths.Daemon, 1)
			err = os.Remove(srcPath)
			if err != nil {
				return fmt.Errorf("Failed to delete symlink %q: %w", srcPath, err)
			}

			err = os.Symlink(newTarget, srcPath)
			if err != nil {
				return fmt.Errorf("Failed to create symlink %q: %w", srcPath, err)
			}
		}
	}

	// Start target.
	fmt.Println("=> Starting the target server")
	err = target.Start()
	if err != nil {
		return fmt.Errorf("Failed to start the target server: %w", err)
	}

	// Cluster handling.
	if clustered {
		if !c.flagClusterMember {
			fmt.Println("=> Waiting for other cluster servers\n")
			fmt.Printf("Please run `lxd-to-incus --cluster-member` on all other servers in the cluster\n\n")
			for {
				ok, err := c.global.asker.AskBool("The command has been started on all other servers? [default=no]: ", "no")
				if !ok || err != nil {
					continue
				}

				break
			}

			fmt.Println("")
		}

		// Wait long enough that we get accurate heartbeat information.
		fmt.Println("=> Waiting for cluster to be fully migrated")
		time.Sleep(30 * time.Second)

		for {
			clusterMembers, err := targetClient.GetClusterMembers()
			if err != nil {
				time.Sleep(30 * time.Second)
				continue
			}

			ready := true
			for _, member := range clusterMembers {
				info, _, err := targetClient.UseTarget(member.ServerName).GetServer()
				if err != nil || info.Environment.Server != "incus" {
					ready = false
					break
				}

				if member.Status == "Evacuated" && member.Message == "Unavailable due to maintenance" {
					continue
				}

				if member.Status == "Online" && member.Message == "Fully operational" {
					continue
				}

				ready = false
				break
			}

			if !ready {
				time.Sleep(30 * time.Second)
				continue
			}

			break
		}
	}

	// Validate target.
	fmt.Println("=> Checking the target server")
	_, _, err = targetClient.GetServer()
	if err != nil {
		return fmt.Errorf("Failed to get target server info: %w", err)
	}

	// Cluster restore.
	if !c.flagClusterMember && clustered {
		fmt.Println("=> Restoring the cluster")

		clusterMembers, err := targetClient.GetClusterMembers()
		if err != nil {
			return fmt.Errorf("Failed to retrieve the list of cluster members")
		}

		for _, member := range clusterMembers {
			fmt.Printf("==> Restoring workloads on server %q\n", member.ServerName)

			op, err := targetClient.UpdateClusterMemberState(member.ServerName, incusAPI.ClusterMemberStatePost{Action: "restore"})
			if err != nil {
				return fmt.Errorf("Failed to restore %q: %w", member.ServerName, err)
			}

			err = op.Wait()
			if err != nil {
				return fmt.Errorf("Failed to restore %q: %w", member.ServerName, err)
			}
		}
	}

	// Confirm uninstall.
	if !c.flagYes {
		ok, err := c.global.asker.AskBool("Uninstall the LXD package? [default=no]: ", "no")
		if err != nil {
			return err
		}

		if !ok {
			os.Exit(1)
		}
	}

	// Purge source.
	fmt.Println("=> Uninstalling the source server")
	err = source.Purge()
	if err != nil {
		return fmt.Errorf("Failed to uninstall the source server: %w", err)
	}

	return nil
}
