# Lumiqo Manager

Lumiqo Updater installs and updates Manager. Manager provisions the Ubuntu
environment and is the sole installer for the Lumiqot Linux distribution. The
package's `lumiqot` CLI manages its services. Manager calls only the versioned
lifecycle interface and dispatches every call as the `lumiqo` service account.

## Clean installation

Use Ubuntu 24.04 with systemd and an existing `lumiqo` account. Prepare the
Worker's credentials and `~lumiqo/.config/lumiqo/worker.json` before enabling
the Worker. Use an absolute `state_dir` outside the package tree. The package
can be installed before this configuration:

```sh
lumiqo-manager reconcile --plan
sudo lumiqo-manager install lumiqot-linux
lumiqo-manager services
```

The package is optional until selected. Subsequent `reconcile --apply` runs
repair required host dependencies and update installed packages. `--restore`
also restores previously installed recommended environment components.
`reconcile --plan` reports package version, available release, integrity,
lifecycle state, journaled recovery, and planned actions without writing to
disk. Reconciliation converges a temporarily drained or stopped service to
its saved preference. Run `lumiqot service enable worker` in the `lumiqo`
user session after configuring the Worker. Automatic updates and explicit
reconciliation work with the Worker disabled.

## Package installation and recovery

The root-owned layout keeps release files immutable to the service account:

```text
/usr/local/bin/lumiqot -> /opt/lumiqo/distribution/current/bin/lumiqot
/opt/lumiqo/distribution/
  current -> versions/v1.2.3-<installation-id>
  versions/
    v1.2.2-<installation-id>/
    v1.2.3-<installation-id>/
  installation.json
  .update.lock
```

Each installation ID identifies a distinct verified copy, including when
repairing the same release. Manager enables lingering for the `lumiqo` user
systemd manager. The package CLI registers user units with stable paths through
`current`. Package binaries and service definitions run with user privileges.

Under the update lock, Manager fetches the stable Linux release and verifies
and stages the archive. Manager journals the transaction, calls the old CLI
to drain for up to 900 seconds and stop for up to 120 seconds, and atomically
changes `current`. Manager then calls the new CLI to start for up to 60 seconds
and checks `status --json` before committing. Both a healthy enabled service
and a stopped disabled service satisfy activation. An aborted drain or stop
triggers convergence of the old package. On candidate failure, Manager
drains and stops the candidate before switching back. If recovery fails,
`installation.json` retains the phase and diagnostics and the next apply
retries recovery. If integrity checks fail for the active CLI, Manager reports
the error and preserves the installation for operator recovery.

Run `sudo lumiqo-manager rollback` to activate the previous verified package
using the same drain, stop, switch, and start sequence. Automatic reconciliation
holds the rolled-back release until a newer release appears. Explicit
`install lumiqot-linux` clears the hold when it deploys the latest version.
While a transaction is pending, rollback first recovers that transaction.

Manager retains two package copies by default, including the active and
previous working copies. To retain more, create
`/etc/lumiqo/distribution.json` with `{"retain_versions": 3}`. Accepted values
are 2 through 20. Cleanup runs after transactions and package repair. Release
rollback switches executables only. Persistent configuration, jobs and other
data stay in place, so releases must keep their state readable by the preceding
release if binary rollback is expected to work.

## Release compatibility

This Manager installs archive format 1 packages with lifecycle protocol 2
and reports `distribution.lifecycle_protocol: 2`. Release it before deploying
the matching Linux package and WinApp to fresh WSL installations. Updater
can install this Manager and run reconciliation using its existing entry
point. The package CLI owns the persistent preferences and reads them during
activation and recovery.
