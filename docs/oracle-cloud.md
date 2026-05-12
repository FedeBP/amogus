# Hosting on Oracle Cloud (Always Free)

This walks through a minimal **Always Free** setup: one small VM, SSH access, Docker, and your bot running with **secrets in a root-only env file** (not in Git).

**What you get:** A VM that stays on 24/7 within free-tier limits. **What costs money:** anything outside Always Free shapes/limits—watch the Oracle quota screens during creation.

---

## 1. Account and region

1. Go to [https://www.oracle.com/cloud/free/](https://www.oracle.com/cloud/free/) and start **Sign Up**.
2. Complete verification (a **credit card** is often required for identity; Always Free resources themselves are $0 if you stay within limits).
3. Pick a **home region** when prompted (e.g. `US East (Ashburn)`). You can’t change it later without migration.

---

## 2. Networking (VCN)

If the console offers **“Start with the default VCN”** or a **Create networking** wizard when you create the instance, use it—it creates:

- A **VCN**
- A **public subnet**
- An **Internet Gateway** and route so the VM can reach the internet (Discord, YouTube, package mirrors)

**Inbound rules:** you only need **SSH** from **your IP** (not the whole internet).

1. **Networking → Virtual cloud networks** → your VCN → **Security lists** (or **Network security groups** if you use NSGs).
2. **Ingress rules:**
   - **Source:** your public IP (search “what is my ip”), with `/32` (e.g. `203.0.113.50/32`).
   - **Protocol:** TCP, **Destination port:** `22`.
3. Save. If your home IP changes, update this rule or you’ll be locked out until you fix it from the console.

**Outbound:** default “allow all” is fine (Discord and Google APIs use HTTPS).

---

## 3. Create the compute instance

1. **Compute → Instances → Create instance**.
2. **Name:** e.g. `amogus-bot`.
3. **Image:** **Oracle Linux 8/9** or **Canonical Ubuntu 22.04** (both work with Docker).
4. **Shape:** choose an **Always Free eligible** shape:
   - **Ampere A1 Flex (ARM):** often **1 OCPU, 6 GB RAM** (fits free tier; you can adjust within your free A1 quota).
   - Or **VM.Standard.E2.1.Micro (AMD)** if you prefer the fixed micro shape.
5. **Networking:** place the instance in your **public subnet** and assign a **public IPv4** address.
6. **SSH keys:** generate or upload your **public** key (you’ll log in as `opc` on Oracle Linux or `ubuntu` on Ubuntu, depending on image).
7. Create the instance. Wait until **Running** and note the **public IP**.

---

## 4. Connect over SSH

```bash
ssh -i /path/to/your_private_key opc@YOUR_PUBLIC_IP
```

(On Ubuntu images the default user is often `ubuntu` instead of `opc`.)

---

## 5. Install Docker

**Oracle Linux 8/9:**

```bash
sudo dnf install -y dnf-plugins-core
sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
sudo dnf install -y docker-ce docker-ce-cli containerd.io
sudo systemctl enable --now docker
sudo usermod -aG docker $USER
# log out and SSH back in so "docker" works without sudo
```

**Ubuntu 22.04:**

```bash
sudo apt update
sudo apt install -y docker.io
sudo usermod -aG docker $USER
newgrp docker
```

Quick check:

```bash
docker run --rm hello-world
```

---

## 6. Put your bot on the server

**Option A — Git clone (private repo):** use a [deploy key](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys) or HTTPS with a token.

**Option B — Upload:** from your PC:

```bash
scp -i /path/to/key -r c:/repos/amogus opc@YOUR_PUBLIC_IP:~/
```

Then on the VM:

```bash
cd ~/amogus
docker build -t amogus .
```

---

## 7. Secrets (not in the image or repo)

Create a file only **root** should read:

```bash
sudo tee /etc/amogus.env >/dev/null <<'EOF'
DISCORD_BOT_TOKEN=paste-discord-bot-token-here
YOUTUBE_API_KEY=paste-youtube-api-key-here
EOF
sudo chmod 600 /etc/amogus.env
```

Never commit this file. Never paste tokens into chat logs.

---

## 8. Run with Docker (manual test)

```bash
docker run --rm --env-file /etc/amogus.env --name amogus amogus
```

Confirm the bot comes online in Discord, then stop with **Ctrl+C**.

---

## 9. Run 24/7 with systemd

Create a unit so the container **restarts** after reboots or crashes:

```bash
sudo tee /etc/systemd/system/amogus.service >/dev/null <<'EOF'
[Unit]
Description=Amogus Discord bot (Docker)
After=docker.service
Requires=docker.service

[Service]
Type=simple
Restart=always
RestartSec=10
ExecStartPre=-/usr/bin/docker rm -f amogus
ExecStart=/usr/bin/docker run --name amogus --rm --env-file /etc/amogus.env amogus
ExecStop=/usr/bin/docker stop amogus

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now amogus.service
sudo systemctl status amogus.service
```

Logs:

```bash
sudo journalctl -u amogus.service -f
```

After you change `amogus.env` or rebuild the image:

```bash
sudo systemctl restart amogus.service
```

---

## 10. Maintenance tips

| Topic | Suggestion |
|--------|------------|
| **Updates** | Periodically `sudo dnf/yum update` or `apt upgrade` and reboot if the kernel updates. |
| **Disk** | Temp audio is under Docker’s writable layer; `&stop` / idle cleanup still applies—watch `df -h` if queues are huge. |
| **Token leak** | If a token ever appears in Git or logs, **rotate** it in Discord and Google Cloud immediately. |
| **SSH lockout** | If your IP changes, update the security list ingress rule for port 22. |

---

## Troubleshooting

| Problem | What to check |
|---------|----------------|
| `docker: permission denied` | Re-login SSH after `usermod -aG docker`, or use `sudo docker`. |
| Bot offline after reboot | `sudo systemctl status amogus` — fix Docker or env file paths. |
| Out of memory | Free tier is small; avoid huge playlists at once or bump RAM if you move off strict free tier. |
| YouTube quota errors | Google Cloud API quotas and billing alerts for the API key. |

---

## Summary

1. Oracle VM in a **public subnet** with **SSH only from your IP**.  
2. **Docker** on the VM.  
3. Build this repo’s **`Dockerfile`**.  
4. Secrets in **`/etc/amogus.env`** (`chmod 600`).  
5. **`systemd`** keeps the container running.

That gives you a **private** server (your VM), **keys not in Git**, and **no need to keep your PC on**.
