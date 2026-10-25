# GPUMon user policy for lab members

GPUMon shows, for every GPU and server, who is using it right now and who used it in the past. It does not
limit or schedule anything. It can only show your name when it can tell that a process is yours. These three
habits make that possible. Usage that cannot be attributed shows up as `unattributed` or `root-shared`.

## Name multipass VMs <username>-<anything>

multipass VMs run as root under the `multipassd` daemon, so the process owner says nothing about you. GPUMon
reads the VM name instead and uses the part before the first `-`:

```
multipass launch --name alice-dev 24.04        # attributed to alice
multipass launch --name testvm 24.04           # unattributed
```

The prefix must be your exact login name on that server.

## Label rootful Docker containers with --label owner=<username>

Containers of the rootful Docker daemon run as root, so GPUMon reads the `owner` label of the container:

```
docker run --gpus all --label owner=alice pytorch/pytorch python train.py
```

Unlabeled containers are attributed to the host's configured default owner, or shown as `unattributed`.
Rootless Docker needs no label: its processes are attributed to you through your UID and your
`/etc/subuid` range.

## Use sudo from your own account instead of logging in as root

When you run `sudo command` from your own account, Linux records your login identity (the "loginuid") on
every process that the command starts, even though they run as root. GPUMon uses it to attribute that usage
to you.

When you log in directly as root (`ssh root@server`), the login identity is root itself, so nobody can tell
who started the processes. GPUMon then shows them as `root-shared`, which helps no one find out whom to ask
about a busy GPU. Please log in with your own account and use `sudo`.

## What GPUMon records

GPUMon records process owners, executable names, CPU, memory, GPU, and disk usage, container names and images,
and VM names. It never records command lines or environment variables. The dashboard shows the last 72
hours; older records are kept for internal audit and are only available to administrators.
