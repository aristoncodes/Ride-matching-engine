# Deploying this to the internet, from nothing

Written for someone who has never deployed anything. It assumes you own no
domain, have no server, and have never used `ssh`.

At the end you will have `https://api.yourdomain.com` serving real traffic, with
TLS, authentication, and a firewall.

**Time:** about 90 minutes, most of it waiting for DNS.
**Cost:** roughly **$5–8/month** plus **$10–15/year** for the domain.

---

## What you are about to build

```
   internet
      │  https:// (443)
      ▼
  ┌────────┐   the ONLY container with a public port
  │ Caddy  │   gets and renews its own TLS certificate
  └───┬────┘
      │ private docker network — nothing below is reachable from outside
      ├──────────────┬──────────────┬───────────────┐
      ▼              ▼              ▼               ▼
   ingestd        requestd       batcherd    matching-engine
      └──────────────┴──────────────┴───────────────┘
                          │
                        redis  (password, no published port)
```

The security model in one line: **one door, and it is locked.**

---

## Step 1 — Buy a domain (~10 min, ~$10–15/year)

Caddy gets you a free TLS certificate from Let's Encrypt, but Let's Encrypt only
issues certificates for names someone owns. There is no way to get a trusted
certificate for an IP address, so the domain is not optional.

Any registrar works. **[Cloudflare](https://dash.cloudflare.com)** and
**[Porkbun](https://porkbun.com)** both sell at cost, which is around $10/year
for a `.com`. Avoid the $1 first-year offers — they renew at $30+.

You do not need a fancy name. `yourname-ridematch.com` is fine; nobody is going
to type it.

> **Careful with free subdomain services** (`.tk`, some dynamic-DNS providers).
> Let's Encrypt rate-limits certificates per registered domain, and a shared
> free domain often has that budget already exhausted by other users.

---

## Step 2 — Rent a server (~10 min, ~$5–8/month)

| Provider | Plan | Cost | Notes |
|---|---|---|---|
| **[Hetzner](https://www.hetzner.com/cloud)** | CX22 — 2 vCPU, 4 GB | **~€4.50/mo** | cheapest; EU/US regions |
| [DigitalOcean](https://www.digitalocean.com) | Basic — 2 vCPU, 4 GB | ~$24/mo | pricier, very good docs |
| [Vultr](https://www.vultr.com) | 2 vCPU, 4 GB | ~$18/mo | many regions |

**Pick 2 vCPU and 4 GB.** Not marketing — sizing from measurements:

- the C++ engine holds the road graph in memory (~27,890 nodes for the Bengaluru
  extract, comfortably under 1 GB, but it grows with graph size);
- Redis is capped at `512mb` by `REDIS_MAXMEMORY`;
- Week 15 measured 10,000 concurrent WebSocket drivers at p99 8.35 ms on a
  laptop, so 2 cores is realistic for a demo and for early real traffic.

1 GB will OOM during the image build. Do not start there.

**When creating it:**
- **Image:** Ubuntu 24.04 LTS
- **Region:** closest to your users (latency is physics; nothing here fixes it)
- **SSH key:** choose "add SSH key" and paste your public key. If you do not have
  one:
  ```bash
  ssh-keygen -t ed25519 -C "you@example.com"   # press Enter three times
  cat ~/.ssh/id_ed25519.pub                    # paste THIS (the .pub one)
  ```
  Never paste the file without `.pub` — that is the private key, and it is the
  entire secret.

Write down the server's public IP.

---

## Step 3 — Point the domain at the server (~5 min, then up to an hour of waiting)

In your registrar's DNS panel, add one record:

| Type | Name | Value | TTL |
|---|---|---|---|
| `A` | `api` | your server's IP | automatic |

That creates `api.yourdomain.com`.

> **Cloudflare users:** set the cloud icon to **DNS only (grey)**, not
> "Proxied (orange)". Proxying breaks the ACME challenge Caddy uses, and
> Cloudflare's free proxy also drops long-lived WebSockets after 100 seconds —
> which would disconnect every driver, repeatedly, in a way that looks like a
> bug in your code.

Check it has taken effect:

```bash
dig +short api.yourdomain.com     # should print your server's IP
```

**Do not continue until this prints the right IP.** Caddy will burn Let's
Encrypt rate limit attempts against a name that does not resolve, and the limit
is 5 failures per hour.

---

## Step 4 — Secure the server (~15 min)

```bash
ssh root@YOUR_SERVER_IP
```

### A non-root user

```bash
adduser --disabled-password --gecos "" deploy
usermod -aG sudo deploy
rsync --archive --chown=deploy:deploy ~/.ssh /home/deploy
```

Running everything as root means one mistake is a total compromise rather than a
bad afternoon.

### Turn off password logins

```bash
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl restart ssh
```

This is the highest-value thing on the page. A public SSH port sees thousands of
password guesses per day, automated, starting within minutes of the IP going
live. Key-only authentication makes that entire category of attack impossible
rather than merely unlikely.

**Before closing this session, open a second terminal and confirm
`ssh deploy@YOUR_SERVER_IP` works.** If you have locked yourself out, you want to
find out while you still have a working session to fix it from.

### Firewall

```bash
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp     # ACME challenge + redirect to HTTPS
ufw allow 443/tcp
ufw allow 443/udp    # HTTP/3
ufw --force enable
ufw status
```

`default deny incoming` is the important line. Everything else is an exception
to it — so a service you accidentally publish later is closed by default rather
than open by default.

### Automatic security updates

```bash
apt update && apt install -y unattended-upgrades
dpkg-reconfigure -plow unattended-upgrades   # choose Yes
```

An unpatched box does not stay yours.

---

## Step 5 — Install Docker (~5 min)

```bash
su - deploy

curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker deploy
newgrp docker

docker --version && docker compose version
```

---

## Step 6 — Get the code and configure it (~10 min)

```bash
git clone https://github.com/aristoncodes/Ride-matching-engine.git
cd Ride-matching-engine
cp .env.example .env
```

Generate the Redis password — **generate it, do not invent one**:

```bash
echo "REDIS_PASSWORD=$(openssl rand -base64 32)" >> .env
```

Then edit `.env` and set:

```bash
RIDEMATCH_DOMAIN=api.yourdomain.com
ACME_EMAIL=you@example.com
```

Check what you are about to start, before you start it:

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml config \
  | grep -A2 published
```

**Only ports 80 and 443 may appear.** If Redis (6379) or any service port shows
up, stop and fix it — that is an open database on the public internet.

This check is not paranoia. The first version of the production overlay used
`ports: []` to close the inherited ports, which does nothing at all: Compose
merges lists by appending. It looked correct and published everything. `!reset []`
is what actually clears them, and `config` is what proves it.

---

## Step 7 — Launch (~10 min, mostly building)

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml up -d --build
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml ps
```

Wait until all six services report `healthy`. First build takes 5–10 minutes —
it compiles the C++ engine from source.

Confirm the certificate was issued:

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml logs caddy | grep -i certificate
```

Confirm authentication is on:

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml logs requestd | grep -i auth
# want: "authentication enabled"
# if it says AUTHENTICATION IS DISABLED, you are running the dev stack — check
# that you passed BOTH -f files
```

---

## Step 8 — Mint the first API key

```bash
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml \
  run --rm keyadmin create --tenant demo --name "demo key" --rate 600
```

Save the printed key immediately. **It is shown exactly once** — only a SHA-256
hash is stored, which is precisely why stealing the database does not hand
anyone working credentials. If you lose it, you rotate; there is no recovery.

Never commit it, never put it in a URL, never paste it into a chat.

---

## Step 9 — Prove it works

From your laptop, not the server:

```bash
export API=https://api.yourdomain.com
export KEY=rmk_...        # the key from step 8

# 1. TLS is real (no -k flag, no warnings)
curl -I $API/v1/ride-requests

# 2. Unauthenticated requests are refused
curl -s -o /dev/null -w "%{http_code}\n" -X POST $API/v1/ride-requests \
  -H 'Content-Type: application/json' \
  -d '{"rider_id":"R-1","pickup":{"lat":12.97,"lng":77.59}}'
# want: 401

# 3. Authenticated requests are accepted
curl -X POST $API/v1/ride-requests \
  -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"rider_id":"R-1","pickup":{"lat":12.9716,"lng":77.5946}}'
# want: 202 and {"request_id":"req_...","status":"PENDING"}

# 4. Nothing else is exposed
curl -s -o /dev/null -w "%{http_code}\n" $API/metrics        # want 404
curl -s -o /dev/null -w "%{http_code}\n" $API/debug/pprof/   # want 404
nc -zv api.yourdomain.com 6379                               # want: refused
```

Then drive real traffic through it, from your laptop:

```bash
cd infrastructure
go run ./cmd/mockdrivers \
  --url wss://api.yourdomain.com/v1/drivers/stream \
  --key "$KEY" --drivers 200 --interval 1s --duration 120s
```

Note `wss://`, not `ws://`. While that runs, submit requests as in step 3 and
watch them match:

```bash
# on the server
docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml logs -f batcherd
# want lines with matched=N and match_rate=1.00
```

---

## Running it

Everything below assumes you are in the repo directory on the server. Save the
typing:

```bash
echo 'alias dc="docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml"' >> ~/.bashrc
source ~/.bashrc
```

| Task | Command |
|---|---|
| Status | `dc ps` |
| Logs | `dc logs -f batcherd` |
| Restart one service | `dc restart requestd` |
| Deploy an update | `git pull && dc up -d --build` |
| New key for a customer | `dc run --rm keyadmin create --tenant acme --name "acme prod"` |
| List a tenant's keys | `dc run --rm keyadmin list --tenant acme` |
| Rotate a key | `dc run --rm keyadmin rotate --key-id <id> --overlap 24h` |
| Revoke immediately | `dc run --rm keyadmin revoke --key-id <id>` |
| Metrics (never public) | `ssh -L 6061:localhost:6061 deploy@IP` then `localhost:6061/metrics` |

The metrics line is worth reading twice. `/metrics` and `/debug/pprof` are on
admin ports that are not proxied and not published — an SSH tunnel is how you
reach them, and that is the point.

### Rotate a key without downtime

```bash
dc run --rm keyadmin rotate --key-id <old-id> --overlap 24h
```

Both keys work during the overlap. Move clients to the new one, then let the old
one expire. Revoking instantly instead is how routine hygiene becomes an outage.

### Back up

```bash
docker run --rm -v ride-matching_redis-data:/data -v $PWD:/backup \
  alpine tar czf /backup/redis-$(date +%F).tar.gz /data
```

That volume holds the API keys and the durable ride-request queue. Copy the
archive **off the server** — a backup that only exists on the machine it is
backing up is not a backup.

---

## What this deployment does not give you

Stated plainly, because a deployment guide that implies more than it delivers is
how people get surprised in production.

- **One machine, so there is downtime.** Any deploy or reboot is an outage of a
  few seconds to a few minutes. There is no redundancy anywhere.
- **Redis is a single instance.** AOF persistence with `appendfsync everysec`
  means up to one second of writes can be lost on a hard crash. Losing the disk
  loses the queue and every API key.
- **No rate limiting at the edge.** Per-key limits are enforced by the
  application, but an unauthenticated flood still reaches Caddy. A real
  deployment puts a CDN or a WAF in front.
- **No alerting.** The metrics and SLOs exist (see
  [../docs/Observability.md](../docs/Observability.md)) but nothing is scraping
  them and nothing pages anyone. You find out it is down when you look.
- **No staging environment.** You are testing in production.
- **Riders cannot cancel and drivers cannot decline.** These were never built.
  For a demo that is fine; no real operator would accept it.

For a portfolio demo, every one of these is an acceptable trade — and being able
to say precisely which corners you cut, and why, is worth more in an interview
than pretending you cut none.

---

## When it does not work

| Symptom | Cause | Fix |
|---|---|---|
| Caddy logs an ACME failure | DNS not propagated, or port 80 blocked | `dig +short api.yourdomain.com`; check `ufw status`; on Cloudflare set DNS to grey/unproxied |
| `curl` says certificate invalid | certificate not issued yet | wait 60s; check `dc logs caddy` |
| 401 with a key you just made | key is for a different tenant, or truncated on copy | `dc run --rm keyadmin list --tenant <t>` |
| "AUTHENTICATION IS DISABLED" in logs | the overlay was not applied | make sure BOTH `-f` files are on the command line |
| Build killed / OOM | server has under 4 GB | resize, or add swap |
| WebSockets drop after ~100s | Cloudflare proxy is on | set DNS-only (grey cloud) |
| Everything healthy, nothing matches | no drivers connected | run `mockdrivers`; `drivers=0` in the batcher log confirms it |

Deeper operational detail: [../docs/Runbook.md](../docs/Runbook.md).
