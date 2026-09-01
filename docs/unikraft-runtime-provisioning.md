# Platform Installation Steps

This document details the steps to install and configure Unikraft Cloud platform (ukp) on a baremetal box with Debian 11.
These steps could also be followed for Debian 13.
Steps are marked with a number to make it easy to know the order and to reference them.

**Unless otherwise specified, all commands must be run as the `root` user.**

## A. Allocate Box

### A1. Collect box requirements

Collect requirements for the baremetal box to use for installing the Unikraft Cloud platform.
Generally this means:

1. Location, region for the box (continent, country, area in the continent / country)
1. Hardware requirements: CPU, memory, disk(s)
1. Box cost, there is generally a per-month fee

It is useful to take a look in existing vendors (steps A2, A3, A4 below) to see the offering, present their offering and decide on the box specs based on requirements and available offerings.

### A2. Identify vendor: Vultr, Hetzner, Latitude, Hivelocity, Leaseweb, AWS

Baremetal boxes are provided by various vendors:

- [Vultr](https://vultr.com/): is quite expensive, but has an easy-to-use interface and is available throughout the world.
- [Hetzner](https://hetzner.com/): is cheap, but is only available in Europe.
  Its interface is a bit more simplistic, but has a degree of customizing the install, see below.
- [Latitude](https://www.latitude.sh/)
- [Hivelocity](https://www.hivelocity.net/)
- [Leaseweb](https://www.leaseweb.com/en/)
- [AWS](https://aws.amazon.com/): is a flagship vendor with a lot of features, but it's quite expensive

### A3. Login to vendor

Have an account on the corresponding baremetal box vendor.
Login to the vendor.

### A4. Identify appropriate baremetal box

As a general requirement, select boxes that have at least 3 disks:

- One disk (any time) is going to be the system install disk.
- Two disks (must be NVMe) are going to be using RAID0 (or RAID1 on use cases that demand reliability) and store the platform data.

#### A4a. Identify appropriate baremetal box in vendor offering: Vultr

For Vultr, to identify the appropriate baremetal box, go to the old interface, which is easier to use.
For this, after logging in to Vultr, click the `Deploy+` button in the top right and the click the link [`Switch back to the old experience for a limited time →`](https://my.vultr.com/deploy/?legacy=true) in the top right.

In the new window, for `Choose Type`, select `Bare Metal`.

Then, for `Choose Location`, select the appropriate location, depending on the request / need.

For `Choose Image`, select `Debian` and then, in the select box below `Debian`, choose `11 x64`.

Then, for `Choose Plan`, select the appropriate box.

> [!NOTE]
> Some plans may be unavailable in a given location.
> Discuss internally or with clients about alternate locations or alternate plans.

#### A4b. Identify appropriate baremetal box in vendor offering: Hetzner

For Hetzner, the best approach is to use the [server auction interface](https://www.hetzner.com/sb/).
Once on the [server auction page](https://www.hetzner.com/sb/), you can scroll the box offerings and choose the best suited one.
You can use the filter rules in the left menu to only present relevant offerings.

### A5. Order box

#### A5a. Order box: Vultr

After choosing the plan, for `Disk Configuration`, if there are 2 system disk (i.e. disks OTHER than the 2 disks used for platform data), choose `RAID1`.
Otherwise, choose `No RAID`.

For `Additional Features`, you can leave the default one (with `IPv6` free) selected.

At `Server Settings`, don’t choose an install script.
At `SSH Keys` select all SSH keys.
For `Sever Hostname` and `Server Label` fill out the name in the Unikraft Cloud convention (e.g. `lax0-tinyfish`, `mum1-flutterflow` etc.) The `Server Label` entry is filled automatically when filling the `Server Hostname` entry.

Then click on the `Deploy Server` button.

Wait for about 5 minutes for the box to be made available.

#### A5b. Order box: Hetzner

Click on the `Order` button in the [Hetzner server auction interface](https://www.hetzner.com/sb/).

In the new page, in the `Server login details` section, keep the selection `Type` to `Public key`, then select all public keys in the `Public key` selection box (use `Ctrl+click` to select all).
Then click `Save`.
Alternatively, if your key is not already in the list, select `New Public key` and paste yours.
Later on, you can manually add other required keys to the machine, and subsequent server orders will remember your key and you can select all of them from the start.

In the new page, validate the box, and then click `Checkout`.

You will receive information on the box and the purchase as an e-mail.

After some 10-30 minutes, you get an e-mail with the server connection details.

#### Open ports

For AWS boxes, make sure to open at least the SSH port and the API HTTPS port 443 in the security group of the box.
If you want to create instances that expose custom TLS ports, those would need to be opened as well.

## B. Basic Setup

### B1. Connect to box

From the web interface of the vendor (and also from e-mail in the case of Hetzner), you can now connect via SSH to the box.
Knowing the IP address of the box, connect with:

```console
ssh root@<IP_address>
```

Replace the `<IP_address>` string with the actual IP address.

You will get a root prompt to the box.

### B2. Create the system partitioning scheme

Before installing the base system, decide on the partitioning scheme, depending on the number of disks, requirements and vendor.

We consider two partitioning decisions:

1. *system partitioning*: creating the boot partition, root filesystem
1. *ukp data partitioning*: storing all platform data

The ukp data partitioning is common to all vendors.
We detail it in section [`D`](#d-data-partition-setup).

The system partitioning scheme depends on the vendor, as some vendors give you more power over the configuration.

#### B2a. Decide on the system partitioning scheme for Vultr

For Vultr the system partitioning scheme is decided in step A5a by the Vultr installer.
The moment you select the disk configuration, you are going to end up with a boot partition and a root partition.
The only difference is whether the root partition is going to be a RAID1 partition split over multiple disks.

> [!NOTE]
> We are working on having an ISO-based installer for Vultr, that will give you full power on the installation and partitioning.

#### B2b. Decide on the system partitioning scheme for Hetzner

Hetzner allows you full control over the partitioning scheme of the box.
This is because Hetzner boxes boot a live image and, with that image, you can configure the system partitioning scheme.

To do that, in the `root` account of the Hetzner box, run the command:

```console
installimage
```

The command will open up a text user interface screen.

In the first prompt, choose `Debian 11`.

Then you are shown a midnight commander like editor.
It is with this editor that you define what the system partitioning scheme would be.
There are two options for the system partitioning scheme:

##### B2b': You have a single disk for system

If you have a single disk for the system, fill the options in the text editor as follows:

```console
DRIVE1 /dev/<drive_name>
SWRAID 0
PART /boot ext3 1024M
PART lvm vg0 all
LV vg0 root / ext4 25G
LV vg0 swap swap swap 8G
```

This will create an boot partition and an LVM volume group `vg0`.
No RAID will be used.

##### B2b'': You have two disks for the system

If you have a two disks for the system, fill the options in the text editor as follows:

```console
DRIVE1 /dev/<drive1_name>
DRIVE2 /dev/<drive2_name>
SWRAID 1
SWRAIDLEVEL 1
PART /boot ext3 1024M
PART lvm vg0 all
LV vg0 root / ext4 25G
LV vg0 swap swap swap 8G
```

This will create a boot partition and an LVM volume group `vg0` using RAID1 (mirroring).

After filling out partition information, also fill out the `HOSTNAME` variable with the name of the `box`.

After this, save using `F2` (or clicking the corresponding button) and exit using `F10` (or clicking the corresponding button).

You will be prompted with a screen detailing the installation steps done.

After that, reboot the system.
Wait for it to reboot and then reconnect with SSH to a now properly installed and partitioned system.

### B3. Check the current setup

Use the command below to check the current setup:

- `lsblk` to check the current partitioning scheme
- `hostname` to check the hostname
- `cat /etc/os-release` to check the distribution (it should be `Debian GNU/Linux 11 (bullseye)`)

### B4. Install system packages

Install required system packages (copy-paste the command below)

```console
apt -yqq update ; apt -yqq upgrade ; apt -yqq dist-upgrade ; apt -yqq full-upgrade ; apt install -yqq jq less zip unzip htop gdb build-essential molly-guard fail2ban tcpdump net-tools apt-file mdadm xfsprogs curl wget ca-certificates lvm2 cryptsetup bc vim tmux tree psmisc man-db manpages finger dbus file; apt-file update ; apt -yqq clean ; apt -yqq autoclean ; apt -yqq autoremove
```

## C. Common User Setup

> [!IMPORTANT]
> While doing the user setup, keep the `root` shell available as a backup, in case you cut yourself off from the box.

### C1. Create `master` user

We aim to have all box access via a new user called `master`.
Firstly, create the new user:

```console
useradd -m -d /home/master -s /bin/bash master
```

### C2. Create password for `master` user

Create the password for `master` user.
Use any good password generator tool to generate a password.
We recommend `pwgen` (must be installed separately):

```console
pwgen -y 32 1
```

Save the password in 1password.
Use the `Infrastructure` vault to create a new `Login` entry.
Name the entry after the box name.
Add the login `master` and the password you just generated.
Save the entry.

Update the password for the `master` user with the command:

```console
echo 'master:<password>' | chpasswd
```

In the command above, replace the `<password>` string with the password you generated for the `master` user (and that you saved in 1passsword).

### C3. Validate the `master` user configuration (if SSH password access is enabled)

To validate the `master` user configuration, connect via SSH to the user:

```console
ssh -l master <IP_address>
```

In the command above, replace the `<IP_address>` string with the IP address of the box.

When prompted, use the password from the above step.

### C4. Copy public SSH keys to the `master` user

In order to access the `master` user via public SSH keys, copy the public SSH keys from the `root` user:

```console
mkdir /home/master/.ssh/
cp /root/.ssh/authorized_keys /home/master/.ssh/authorized_keys
sudo chown -R master:master /home/master/.ssh
```

If you are using another user to SSH into the machine (e.g., `admin` for AWS machines), then copy the keys from there, i.e.:

```console
cp /home/admin/.ssh/authorized_keys /home/master/.ssh/authorized_keys
```

### C5. Validate the `master` SSH access

To validate the `master` user SSH access, connect via SSH to the user:

```console
ssh -l master <IP_address>
```

In the command above, replace the `<IP_address>` string with the IP address of the box.

If the configuration is correct, you won’t get any password prompt.
You will get direct access to a remote shell for the `master` user on the box.

### C6. Make the `master` user able to get `root` privileges

We want to only use the `master` user for remote access.
For that, after connecting to the `master` user, we need to transition to the `root` user.
For this, we provide the `master` use with the ability to get `root` privileges via `sudo`.
Use the command below for that:

```console
adduser master sudo
```

### C7. Validate `master` user privileges

To validate that the `master` user can get `root` privileges, use, as the `master` user:

```console
sudo su
```

When prompted for a password, use the `master` user password.
If all going OK, you will be able to access a `root` shell.

### C8. Disable direct access to the `root` user

We want to only enable the `master` user as a remote and local user access, not the `root` user.
We disable local password-based access to the `root` user:

```console
usermod -L root
```

To disable SSH access to the `root` user and password-based SSH access to the box, edit the `/etc/ssh/sshd_config` file.
Make sure the following lines are present in the file (and not any other conflicting lines):

```console
PermitRootLogin no
PasswordAuthentication no
```

> [!NOTE]
> For Vultr boxes, also edit the file `/etc/ssh/sshd_config.d/50-cloud-init.conf` and make sure the line in that file is either commented out or updated to:
>
> ```console
> PasswordAuthentication no
> ```

After these changes, restart the SSH server:

```console
systemctl restart ssh
```

> [!NOTE]
> Note that for Vultr you may lose 3-5 minutes of connectivity after restarting the SSH server (not sure why that happens).
> It will be fine after 3-5 minutes.

### C9. Validate `master`-only SSH access

To validate `master`-only SSH access, run the command to try accessing the `root` account:

```console
ssh -l root <IP_address>
```

In the command above, replace the `<IP_address>` string with the IP address of the box.

If all OK, you will be denied access to the `root` account.
You will get a message such as `Permission denied (publickey).`

Then, run the command to access the `master` user:

```console
ssh -l master <IP_address>
```

In the command above, replace the `<IP_address>` string with the IP address of the box.

If all OK, you will have access to a shell for the `master` user on the remote box.
Also access the `root` account with the command:

```console
sudo su
```

When prompted for a password, use the `master` user password.
If all going OK, you will be able to access a `root` shell.

### C10. (Optional, AWS) Remove `admin` user

If the machine is on AWS, it comes with an `admin` user out of the box that you have used to first connect to it.
We no longer need it - remove it:

```console
deluser admin
rm -rf /home/admin
```

## D. Data Partition Setup

### D1. Create an MD RAID array for ukp data

> [!IMPORTANT]
> If you have used all the disks for system partitioning, then you just shouldn't create another MD RAID array.
> You just need to run `lvcreate -n ukp-data -l 100%FREE vg0` to create the logical volume for ukp data and you should skip directly to step `D6. Validate the LVM setup`.

Create an MD RAID array with all disks used for ukp data.
The command is:

```console
mdadm --create --verbose /dev/md<X> --level=0 --raid-devices=<NUM> /dev/<device1> /dev/<device2> ...
```

The items `<X>`, `<NUM>`, `<device1>`, `<device2>` are to be replaced by the correct value depending on the current setup:

- `<X>` is the id of the first available MD RAID device, such as `0` or `1` or `2`
- `<NUM>` is the number of devices in the RAID array
- `<device1>`, `<device2>` ... are the actual devices

For example, if we are using two devices and this is the first MD RAID device, a possible command is:

```console
mdadm --create --verbose /dev/md0 --level=0 --raid-devices=2 /dev/nvme0n1 /dev/nvme1n1
```

If we are using two devices and this is the second MD RAID device (because the first RAID device was used by a RAID setup for the system partitioning scheme), a possible command is:

```console
mdadm --create --verbose /dev/md1 --level=0 --raid-devices=2 /dev/nvme0n1 /dev/nvme1n1
```

If we are using four devices and this is the third MD RAID device (because the first is a Hetzner setup with a RAID1 for the boot partition and another RAID1 setup for the volume group root partition), a possible command is.

```console
mdadm --create --verbose /dev/md2 --level=0 --raid-devices=4 /dev/nvme0n1 /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1
```

Adapt the above to your box configuration and needs.

> [!NOTE]
> The above commands use RAID0 (stripping) for the data partition.
> If required / relevant, RAID1 (mirroring) could also be used for the data partition.
> In which case the option would be `--level=1` instead of `--level=0`.

### D2. Validate and extract MD configuration

Use the command below to list the MD RAID configuration and validate it:

```console
mdadm --detail --scan
```

### D3. Make the MD RAID configuration persistent

To make the MD RAID configuration persistent, add the contents of the command above to the `/etc/mdadm/mdadm.conf` file.
Edit the file and add the contents.

> [!IMPORTANT]
> Make sure that each MD RAID entry is added once (and only once) in `/etc/mdadm/mdadm.conf`.

### D4. Update initrd

Update initrd to be able to use the MD RAID configuration at boot time:

```console
update-initramfs -k all -u
```

### D5. Create LVM setup for ukp data

We create an LVM (*Logical Volume Manager*) setup for ukp data to have flexibility in adding new disks or partitions.

We create a volume group on top of the MD RAID device and create a logical volume filling the entire volume group.
We need the MD RAID device name and the volume group name:

- The MD RAID device name is the one in step [`D1`](#d1-create-an-md-raid-array-for-ukp-data).
- The volume group name is `vg0` if no previous volume exists (with the `vg0` name).
  The name is `vg1` if the `vg0` name exists (as is the case with Hetzner-based installations that use a volume group for the system partitioning scheme).

With the MD RAID device name and volume group name figure out, create the volume group and logical volume, by running the commands below (**Note**: Update the volume group name and the MD RAID device name to your setup - from `vg0` to something specific to your setup, and from `/dev/md1` to something specific to your setup):

```console
pvcreate /dev/md<X>
vgcreate vg<X> /dev/md<X>
lvcreate -n ukp-data -l 100%FREE vg<X>
```

The partition device for the ukp data is `/dev/mapper/vg<X>-ukp--data`, where `<X>` is the ID of the volume group used for ukp data, depending on the configuration.

### D6. Validate the LVM setup

Use the commands below to validate the LVM setup:

```console
pvs
vgs
lvs
dmsetup info -c
ls -l /dev/mapper/vg*-ukp--data
```

### D7. Create partition encryption password

For partition encryption (next step) we need a password.
As with step `C2. Create password for master user`, use any good password generator tool to generate a password.
We recommend `pwgen` :

```console
pwgen -y 32 1
```

Save the generated password in 1password, in the same entry as the one storing the information for the `master` password.
Add a new password field and store the password in that field.
Name the field `LUKS encryption password`.
See similar entries in the `Infrastructure` vault.

### D8. Create LUKS setup

We want to encrypt the ukp data partition for security reason.
We use [LUKS](https://en.wikipedia.org/wiki/Linux_Unified_Key_Setup) (*Linux Unified Key Setup*) for this.
We will create a LUKS setup on top of the `/dev/mapper/vg<X>-ukp--data` partition, where `<X>` is the ID of the volume group used for ukp data, depending on the configuration.

Use the commands below to create the LUKS setup (update the data partition name to your configuration):

```console
cryptsetup luksFormat /dev/mapper/vg<X>-ukp--data
cryptsetup open /dev/mapper/vg<X>-ukp--data ukp-encrypted
```

When running the first command you will be prompted to answer `YES` (capital letter).
Do that.
Then, you will be asked to provide the partition encryption password twice.
Use the password from step `D7. Create partition encryption password`.

You will end up with the encrypted partition device named `/dev/mapper/ukp-encrypted`.

### D9. Validate LUKS setup

Use the commands below to validate the LUKS setup:

```console
lsblk
dmsetup info -c
cryptsetup status /dev/mapper/ukp-encrypted
```

### D10. Format encrypted partition

Use the command below to format the encrypted partition device with XFS.
Replace `<NUM_RAID_DEVICES>` with the number of devices part of the RAID array (e.g. `2`):

```console
mkfs.xfs -d su=256k,sw=<NUM_RAID_DEVICES> /dev/mapper/ukp-encrypted
```

> [!IMPORTANT]
> If you get warnings stating that the stripe unit/width is not the same as the volume, let `mkfx.xfs` figure it out on its own:
>
> ```console
> mkfs.xfs /dev/mapper/ukp-encrypted -f
> ```

### D11. Validate partition formatting

Use the command below to validate the formatting of the encrypted partition `/dev/mapper/ukp-encrypted` using XFS:

```console
file -s $(readlink -f /dev/mapper/ukp-encrypted)
```

You will get an output similar to `SGI XFS filesystem data (blksz 4096, inosz 512, v2 dirs)`.

## E. Mount Data Partition Configuration

### E1. Add entry to `/etc/fstab`

To assist in mounting of the partition, edit the `/etc/fstab` file and add the following entry:

```console
/dev/mapper/ukp-encrypted /var/lib/ukp xfs defaults,uquota,gquota,pquota,discard,noauto 0 2
```

Note the `noauto` option.
The partition is not going to be mounted automatically, but manually.
We do that because we need to provide the encryption password.
We could automate this action, but that would require storing the encryption password locally, and that’s a security risk.

### E2. Create mounting script

Create the `mount-ukp-data.sh` script that we will use the mount the partition.
Create it either in `/home/master/` or in the `/root/` directory.
The contents of the script are:

```console
#!/bin/sh

initial_device="/dev/mapper/vg<X>-ukp--data"
encrypted_name="ukp-encrypted"
encrypted_device="/dev/mapper/$encrypted_name"
mount_point="/var/lib/ukp"

if test ! -b "$initial_device"; then
    echo "No initial block (unencrypted) device $initial_device" 1>&2
    exit 1
fi

cryptsetup open "$initial_device" "$encrypted_name"
mount "$mount_point"
```

> [!TIP]
> In the script, if required, update the line `initial_device="/dev/mapper/vg<X>-ukp--data"` with the correct path to the logical volume name.

Make sure the script has execution permissions:

```console
chmod a+x mount-ukp-data.sh
```

### E3. Close the encrypted partition

Before running the mount script, we close the encrypted partition.
The mount script also opens the encrypted partition, so we need it closed beforehand:

```console
cryptsetup close ukp-encrypted
```

### E4. Create mount point

The mount point for the ukp data partition is `/var/lib/ukp`.
Create the mount point using:/

```console
mkdir /var/lib/ukp
```

### E5. Run the mount script

Run the mount script using:

```console
./mount-ukp-data.sh
```

You will be prompted for the LUKS encryption password.
Provide it.
If everything works OK, the LUKS encrypted partition will be mounted in `/var/lib/ukp`, readying it for the Unikraft Cloud Platform install.

### E6. Validate the ukp data partition mounting

Use the command below to validate the ukp data partition mounting:

```console
mount | grep ukp-encrypted
```

You should get an output such as:

```console
/dev/mapper/ukp-encrypted on /var/lib/ukp type xfs (rw,relatime,attr2,inode64,logbufs=8,logbsize=32k,sunit=1024,swidth=2048,usrquota,prjquota,grpquota)
```

## F. DNS Configuration

### F1. Configure DNS

The platform API will be provided as a DNS entry.
For Unikraft-managed DNS entries, the format is `api.<metro><ID>-<client>.unikraft.cloud` (e.g. `api.lax1-tinyfish.unikraft.cloud`).

Clients / users will need to provide and configure their DNS entry.
As a client / user, create a wildcard DNS `A` entry that maps the entry `*.<domain-name-for-ukp>` to the IP address of the box, where `<domain-name-for-ukp>` is the domain name chosen / configured for the box.
An example is the mapping of `*.fsn2.superserver.cloud` to `147.208.0.32`.

### F2. Validate DNS configuration

To validate the DNS configuration, target the domain name `api.<domain-name-for-ukp>`, such as [`api.lax1-tinyfish.unikraft.cloud`](http://api.lax1-tinyfish.unikraft.cloud) or `api.fsn2.dreamflow.cloud`.
Use a DNS client such as `host`:

```console
host api.lax1-tinyfish.unikraft.cloud
host api.fsn2.dreamflow.cloud
```

Similarly, use that domain name for SSH connections to the remote box:

```console
ssh -l master api.lax1-tinyfish.unikraft.cloud
ssh -l master api.fsn2.dreamflow.cloud
```

### F3. (Optional) Update hostname

To update the hostname, you need to run:

```console
 hostnamectl set-hostname <hostname>
```

The `<hostname>` string depends on the DNS configuration:

#### F3a. Unikraft-managed DNS entries

```text
<metro><ID>-<client>
```

e.g., `lax1-tinyfish`.
Go into `/etc/hosts` and delete any occurrence of the old hostname (i.e., anything related to `127.0.0.1` other than `localhost`). Add a line with the following:

```text
127.0.1.1 <hostname>.unikraft.cloud <hostname>
```

#### F3b. Client-managed DNS entries

`<hostname>` = the domain name chosen by the client / user, e.g., `fsn2.dreamflow.cloud`.

Go into `/etc/hosts` and delete any occurrence of the old hostname (i.e., anything related to `127.0.0.1` other than `localhost`). Add a line with the following:

```text
127.0.1.1 <hostname>
```

> [!IMPORTANT]
> On AWS or other cloud providers, these changes alone might not be persistent because `cloud-init` might overwrite the `/etc/hosts` file.
> This can be easily proven if your `/etc/hosts` file has a banner similar to the one below.
> In that case, you need to edit `/etc/cloud/cloud.cfg` and remove/comment the line containing `update_etc_hosts`.
> Additionally, you could also search the files inside `/etc/cloud/cloud.cfg.d` for the `manage_etc_hosts` option and remove/comment that as well.
>
> ```text
> # Your system has configured 'manage_etc_hosts' as True.
> # As a result, if you wish for changes to this file to persist
> # then you will need to either
> # a.) make changes to the master file in /etc/cloud/templates/hosts.debian.tmpl
> # b.) change or remove the value of 'manage_etc_hosts' in
> #     /etc/cloud/cloud.cfg or cloud-config from user-data
> #
> ```

## G. Pre-Install Configuration

### G1. Have (client) username, UUID and token available

You need a client token to be able to use the platform install.
If you don't already have an account, create a user account via the [sign-up form on the Unikraft Cloud website](https://console.unikraft.cloud/signup).

### G2. Have platform install token available

Installing the platform depends on an install token;
this is a different token than the user token that you get after signing up.

**The install token is generated and provided by Unikraft, via a secure channel.**

### G3. Prepare install environment variables

Before installing, we need to define a bunch of environment variables:

- `INSTALL_TOKEN`: The install token from step `G2. Have platform install token available`
- `HOSTNAME`: The hostname from step `F3. Update hostname`
- `DNS_ZONE_API`: Top-level DNS domain for API
- `DNS_ZONE_APP`: Top-level DNS domain for instances
- `NET_IFACE`: Name of public network interface (`ip a sh`)
- `CLOUDFLARE_EMAIL`: Email address referenced by CloudFlare for all SSL certificates
- `CLOUDFLARE_TOKEN`: CloudFlare access token which has read/write permissions to `DNS_ZONE_API`
- `NET_MODE`: Network mode; options are `subnets` and `isolates`.
  Default is `subnets`.
  **Note**: Currently only `isolates` is supported
- (optional) `NET_COUNT`: Maximum number of devices / instances per network.
  Default is `255`
- (optional) `OTEL_COLLECTOR_ADDR`: External OpenTelemetry endpoint.
  Unsetting disables sending logs
- (optional) `OTEL_COLLECTOR_TOKEN`: External OpenTelemetry access token

A sample configuration of these environment variables is below (replace with your own values):

```console
export INSTALL_TOKEN=<REDACTED>
export HOSTNAME=<HOSTNAME>
export DNS_ZONE_API=unikraft.cloud
export DNS_ZONE_APP=unikraft.app
export NET_IFACE=<NET_IFACE>
export CLOUDFLARE_EMAIL=monkey+cloudflare-@unikraft.io
export CLOUDFLARE_TOKEN=<REDACTED>
export NET_MODE=isolates
export OTEL_COLLECTOR_ADDR=collect.unikraft.io
export OTEL_COLLECTOR_TOKEN=Mbicwu7Eo4b6ejdP4gQGVaHZlyp6Zq44ddagTsdcIrX878cvICYM6k4Fuoy2aui4
```

You can create a `ukc.install.config` file to dump all of these, and then bring them into your shell with:

```console
source ukc.install.config
```

## H. Install and Configuration

### H1. Install the platform

To install the platform (and make the initial configuration), run (depending on what version you want to install):

```console
# stable
curl -sSfL https://install.unikraft.cloud?token="$INSTALL_TOKEN" \
	| sh
# preview
curl -sSfL https://install.unikraft.cloud?token="$INSTALL_TOKEN" \
	| INSTALL_PREVIEW=y sh
# staging
curl -sSfL https://install.unikraft.cloud?token="$INSTALL_TOKEN" \
	| INSTALL_STAGING=y sh
```

> [!NOTE]
> If the URL above is not available, try the staging installation URL:
> `curl -sSfL https://install.stage.unikraft.cloud?token="$INSTALL_TOKEN" | sh`

This will result in installing the platform packages and preparing the initial configuration files.
Platform services will not be started (yet).

> [!NOTE]
> `INSTALL_STAGING` variable tells the installer to set up staging sources, but it does not automatically install staging versions of components.
> It only installs `preview` for `ukp-platform`.
>
> To install staging versions, you can list available versions with `apt list -a {component}` and install specific version with `apt install {component}={version}`.
> The `staging` package repository is setup with lower priority than `stable` and `preview`.
> If you want to automatically upgrade all packages to `staging` (not advised though), you should update the priority to something **above 500**:
> ```bash
> vim /etc/apt/preferences.d/unikraft-cloud-staging.pref
> apt install --only-upgrade "ukp*"
> ```

If you want to update an existing installation to staging, but it did not have access to staging packages, you can add them manually to apt (make sure to adjust for `trixie` (deb13) or `bullseye` (deb11)):

```
# /etc/apt/preferences.d/unikraft-cloud-staging.pref 
Package: *
Pin: origin pkg.unikraft.com
Pin: release c=staging
Pin-Priority: 600

...

# /etc/apt/sources.list.d/unikraft-cloud-staging.sources
Types: deb
URIs: https://pkg.unikraft.com/debian/cloud-staging/
Suites: trixie
Components: staging
Signed-By: /usr/share/keyrings/unikraft-cloud-staging.gpg
```

### H2. Validate the platform installation

A direct way to validate the platform installation is to check the post-install directories and configuration files.
Relevant entries are:

- `/var/lib/ukp`: This is where all platform data is stored: images, instance filesystems, snapshots, instance configuration - this is the ukp data partition
- `/usr/lib/ukp`: Platform binaries and support tools
- `/usr/local/openresty`: OpenResty configuration files and scripts
- `/var/log/ukp`: Logging information
- `/var/run/ukp`: Runtime information such as PID files and local domain (UNIX) sockets
- `/etc/ukp.conf`: Main configuration file

You can also check the local ukp packages:

```console
dpkg -l 'ukp-*'
```

It's good to take a look at the `/etc/ukp.conf` file, used in the next step:

```console
cat /etc/ukp.conf
```

### H3. Configure ukp

Update the configuration of ukp in `/etc/ukp.conf`.
 There are some mandatory updates to the configuration that you must do, to get to contents such as the ones below:

```text
[...]

# List of mount points to wait for during system startup before starting UKP
# services. This option enables Unikraft data to be stored on encrypted volumes
# that need to be manually mounted.
UKP_CUSTOM_MPOINTS=()
UKP_CUSTOM_MPOINTS+=("/var/lib/ukp")

## Platform daemon
# Extra arguments to daemon (use bash array notation!)
UKPD_EXTRA_ARGS=()
UKPD_EXTRA_ARGS+=("--vmm-initrd-map-shared")
UKPD_EXTRA_ARGS+=("--stor-max-total-volume-mb=<fs-size>")

[...]

AGENT_PULL_INCLUDE="<clientname>,official"
```

In the contents above, replace the `<clientname>` string with the client name (for the `AGENT_PULL_INCLUDE` line).
Also, replace `<fs-size>` with the size of the encrypted filesystem, in MB.
You can find it like this:

```console
df /dev/mapper/ukp-encrypted -B M
```

Explanations for the above lines:

- `UKP_CUSTOM_MPOINTS` is mandatory to prevent the platform from starting (at reboot).
  It would only start after the mounting of the `/var/lib/ukp` directory, done via the [`mount-ukp-data.sh`](http://mount-ukp-data.sh) script (as presented in step `E5. Run the mount script`).
- `UKPD_EXTRA_ARGS` is important to share the initrd of multiple instances created from the same initial ramdisk image, saving memory.
- `AGENT_PULL_INCLUDE` details the namespaces that the image agent can pull images from.
  It must always include `official` and it must also include the client (namespace) name for the client using the box.

Other ukp configurations depend on requirements, requests or preferences.
This is to be discussed with the client and with the platform team.

### H4. Configure platform user(s)

The platform is available to configured users.
A user is defined by a username, uuid, password and permissions, roles.
All these information go to the `/var/lib/ukp/data/users.json` file.

For this step you need the user information from step [`G1`](#g1-have-client-username-uuid-and-token-available).

First, create the `/var/lib/ukp/data/` directory:

```console
mkdir /var/lib/ukp/data
```

Then, fill the user information in the `/var/lib/ukp/data/users.json` file.
A sample configuration, with a single user, is below:

```json
[
        {
                "uuid": "<uuid>",
                "name": "<username>",
                "auth_token": "<token>",
                "network_id": 0,
                "autoscale": {
                        "min_size": 0,
                        "max_size": 16
                },
                "vmdb": {
                        "max_vcpus": 8,
                        "max_memory_mb": 81920,
                        "max_instances": 1280
                },
                "net": {
                        "max_service_groups": 1021,
                        "max_services": 1021
                },
                "vmm": {
                        "max_vcpus": 1024,
                        "max_memory_mb": 387216
                },
                "stor": {
                        "max_volumes": 10240,
                        "max_volume_mb": 204800,
                        "max_total_volume_mb": 1800000
                }
        }
]
```

In the above file, replace `<uuid>`, `<username>` and `<token>` with the appropriate values.

### H5. Reboot the box

To finalize the ukp installation (and configuration), reboot the box:

```console
reboot
```

Because of molly-guard running, you may be asked to provide the hostname.
Provide the hostname and the box will reboot.

Wait for about 3-5 minutes for the box to become available.

## I. Validate and Post-Install

### I1. Reconnect to the box

Reconnect to the box via SSH.
Firstly connect to the box using the `master` user via SSH:

```console
ssh -l master api.<domain-name-for-upkp>
```

In the command above, replace `<domain-name-for-ukp>` with the box domain name.

Then, get `root` privileges:

```console
sudo su
```

When prompted for a password, provide the password for the `master` account, stored in 1password.

You will end up with a `root` shell.

### I2. Validate that main ukp services are not started

Because we haven’t yet mounted the encrypted ukp partition, the main ukp services are not started.
Use the command below to check that:

```console
systemctl list-units 'ukp-*'
```

You will see that the `ukp-openresty.service` and the `ukp-platform.service` services are not yet started.

### I3. Mount the encrypted data partition

Run step [`E5`](#e5-run-the-mount-script).

### I4. Validate the ukp data partition mounting

Run step [`E6`](#e6-validate-the-ukp-data-partition-mounting).

### I5. Validate that the main ukp services are running

2-3 minutes after the data partition was mounted, the main ukp services should be running.
Validate using:

```console
systemctl list-units 'ukp-*'
```

You will see that the `ukp-openresty.service` and the `ukp-platform.service` services are started.

### I6. (Optional, AWS) Update OpenResty configuration

If you are using a different SSH port to connect to the machine (i.e., `ssh -p 44`), you need to update the Nginx configuration to not pick up connections on that port and let the SSH daemon handle them.

Unlink the `/var/lib/ukp/data/openresty/nginx.conf.template` symbolic link and create a copy of it from the initial `/usr/lib/ukp/platform/openresty/conf/nginx.conf.template` file:

```console
unlink /var/lib/ukp/data/openresty/nginx.conf.template
cp /usr/lib/ukp/platform/openresty/conf/nginx.conf.template /var/lib/ukp/data/openresty/nginx.conf.template
```

We need to remove the symbolic link, because an update to the OpenResty package may update the `/usr/lib/ukp/platform/openresty/conf/nginx.conf.template` file (and we would lose the updates).

Edit the newly created `/var/lib/ukp/data/openresty/nginx.conf.template` file and make sure that the line containing `any_port_ignore` has the port you are using for SSH, e.g.:

```text
any_port_ignore 22,44;
```

After that, restart OpenResty:

```console
systemctl restart ukp-openresty.service
systemctl status ukp-openresty.service
```

## J. Test Install

### J1. Validate basic ukp API functionality

To validate basic ukp API functionality, on a client station with KraftKit installed, define two variables:

- `UKC_TOKEN`: the user access token
- `UKC_METRO`: the API endpoint for the platform install on the box

The user access token is the token in the `/var/lib/ukp/data/users.json` file, configured in step [`H4`](#h4-configure-platform-users).

Use commands such as the ones below:

```console
export UKC_TOKEN=<token>
export UKC_METRO=https://api.<domain-name-for-upkp>/v1
```

In the command above, replace the `<token>` string with the value of the user token in the `/var/lib/ukp/data/users.json` file.

Then, run the commands below to list instances, images and volumes:

```console
kraft cloud instance list
kraft cloud image list
kraft cloud volume list
```

The commands should print out only headers, without contents (except maybe for `image list`).
But that means that the API and the platform are working OK.

### J2. Validate instance deployment on the platform

To validate instance deployment on the platform, firstly define the `UKC_TOKEN` and `UKC_METRO` variables as in the step above.

Then clone or navigate to a local clone of the [`unikraft-cloud/examples` repository](https://github.com/unikraft-cloud/examples).
Go to the `http-go1.21/` directory.
And then deploy a sample Go application:

```console
kraft cloud deploy --name http-go --subdomain http-go --memory 512Mi --port 443:8080 .
```

After a successful deployment, get information on the instance:

```console
kraft cloud instance info http-go
```

From the output above, extract the `fqdn` entry.
And query the instance:

```console
curl https://<fqdn>
```

where the `<fqdn>` string is to be replaced with the value above.

If everything is OK, then congratulations:
You now have a successful platform install.
