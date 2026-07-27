# x86 test box (Azure)

Scratch machine for the testing that cannot be done on arm64 — the real Proxmox
packages are amd64-only.

## Connect

```bash
ssh -p 2222 -i ~/.ssh/multiprox_azure azureuser@4.225.205.155
```

**Use port 2222, not 22.** Subscription governance (Defender for Cloud / the
*Microsoft Cloud Security Benchmark (nonprod)* assignment) periodically deletes
NSG rules allowing inbound 22. It was observed removing one within minutes: the
rule vanished, `az network nsg rule show` returned `NotFound`, and only
`DenyAllInBound` remained. A non-standard port is not policed the same way.

The key is dedicated to this box (`~/.ssh/multiprox_azure`), not a reused one.

### How 2222 is configured

Ubuntu 24.04 socket-activates sshd, so `sshd_config` is not where the port
lives — `/etc/systemd/system/ssh.socket.d/override.conf` is:

```ini
[Socket]
ListenStream=
ListenStream=22
ListenStream=2222
BindIPv6Only=both
```

`BindIPv6Only=both` matters. Without it, a bare `ListenStream=22` after the
reset binds `[::]` only, and every IPv4 connection is refused — which looks like
a firewall problem but is not. Do **not** list `0.0.0.0:22` and `[::]:22`
separately either; they collide and `ssh.socket` fails to start.

### There are TWO NSGs — rules must exist in both

This costs an hour if you miss it. Traffic has to be permitted by the **subnet**
NSG *and* the **NIC** NSG; an allow in only one is a deny.

| NSG | Attached to |
|---|---|
| `multiprox-x86NSG` | the NIC (created by `az vm create`) |
| `multiprox-x86VNET-multiprox-x86Subnet-nsg-swedencentral` | the **subnet** (created by governance, *after* the VM) |

The subnet NSG did not exist when the VM was created — `az network vnet subnet
list` reported `None`. Governance added it later with **zero rules**, so
`DenyAllInBound` took effect and SSH died mid-session with nothing on our side
having changed. The NIC NSG still showed a correct ALLOW, which makes this look
like anything but a firewall problem.

Check both, and note that `list-effective-nsg` returns one entry **per NSG** —
if you see two blocks of rules, that is two NSGs, not duplicated output:

```bash
NIC=$(az vm show -g rg-multiprox -n multiprox-x86 \
        --query "networkProfile.networkInterfaces[0].id" -o tsv)
az network nic list-effective-nsg --ids "$NIC" \
  --query "value[].{nsg:networkSecurityGroup.id, rules:effectiveSecurityRules[?access=='Allow' && direction=='Inbound'].name}"
```

To add a rule to the subnet NSG:

```bash
az network nsg rule create -g rg-multiprox \
  --nsg-name multiprox-x86VNET-multiprox-x86Subnet-nsg-swedencentral \
  -n allow-ssh-2222 --priority 1100 --direction Inbound --access Allow \
  --protocol Tcp --source-address-prefixes "$(curl -s -4 ifconfig.me)/32" \
  --destination-port-ranges 2222
```

### Locked out?

`az vm run-command` reaches the VM through the Azure agent with no inbound
networking at all, so it works even when every NSG rule is gone:

```bash
az vm run-command invoke -g rg-multiprox -n multiprox-x86 \
  --command-id RunShellScript --scripts 'ss -tln | grep :22'
```

To re-add an NSG rule (bump the priority if the name is taken):

```bash
az network nsg rule create -g rg-multiprox --nsg-name multiprox-x86NSG \
  -n allow-ssh-alt --priority 1100 --direction Inbound --access Allow \
  --protocol Tcp --source-address-prefixes "$(curl -s -4 ifconfig.me)/32" \
  --destination-port-ranges 2222
```

## What it is

| | |
|---|---|
| Resource group | `rg-multiprox` (swedencentral) |
| VM | `multiprox-x86`, `Standard_B16as_v2` |
| CPU | AMD EPYC 7763, 16 vCPU, **x86_64** |
| RAM | 64 GB |
| OS | Ubuntu 24.04.4 LTS |
| OS disk | 200 GB Premium SSD |
| Data disks | `sdb`, `sdc`, `sdd` — 3 × 64 GB, raw, for Ceph OSDs |
| Inbound | SSH (22) from `45.81.171.73/32` only |

Three genuinely raw disks means the OSD path can use the *real disks* route from
[block-storage.md](block-storage.md) rather than loop devices — no loop caveats,
and it exercises what a production deployment would actually do.

## Cost control

B-series is burstable and this is a paid subscription, so **deallocate it when
you are not using it**. Stopping from inside the guest does not stop billing;
`deallocate` does.

```bash
az vm deallocate -g rg-multiprox -n multiprox-x86   # stops compute billing
az vm start      -g rg-multiprox -n multiprox-x86   # resume (public IP may change)
```

Disks continue to bill while deallocated. To remove everything:

```bash
az group delete -n rg-multiprox --yes --no-wait
```

## Burstable caveat, stated up front

`B16as_v2` accrues CPU credits and throttles to a baseline once they are spent.
That is fine for correctness testing — does the cluster form, do OSDs come up,
does the operator reconcile — and misleading for anything about throughput or
recovery timing. Do not read Ceph performance numbers off this box.

## If your IP changes

The NSG allows a single address. To update it:

```bash
az network nsg rule update -g rg-multiprox --nsg-name multiprox-x86NSG \
  -n allow-ssh-from-admin --source-address-prefixes "$(curl -s -4 ifconfig.me)/32"
```
