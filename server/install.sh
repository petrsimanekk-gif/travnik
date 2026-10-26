#!/usr/bin/env bash
# Stínko CRM – instalace a aktualizace na Ubuntu VPS (24.04 / 26.04).
#
#   curl -fsSL https://raw.githubusercontent.com/petrsimanekk-gif/travnik/claude/bold-brown-vhwapp/server/install.sh | bash -s -- crm.primashade.cz
#
# Opakované spuštění = aktualizace na poslední verzi z GitHubu (data zůstanou).
set -euo pipefail

DOMAIN="${1:-crm.primashade.cz}"
REPO="${STINKO_REPO:-https://github.com/petrsimanekk-gif/travnik.git}"
BRANCH="${STINKO_BRANCH:-claude/bold-brown-vhwapp}"
BASE=/opt/stinko
say(){ printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

[ "$(id -u)" = 0 ] || { echo "Spusťte jako root (nebo přes sudo)."; exit 1; }
export DEBIAN_FRONTEND=noninteractive
# čerstvý VPS často zrovna instaluje aktualizace – počkat na zámek apt místo chyby
echo 'DPkg::Lock::Timeout "900";' > /etc/apt/apt.conf.d/90stinko-lock

say "Systémové balíčky"
apt-get update -qq
apt-get install -y -qq curl git ca-certificates gnupg ufw sqlite3 restic unattended-upgrades debian-keyring debian-archive-keyring apt-transport-https >/dev/null
dpkg-reconfigure -f noninteractive unattended-upgrades >/dev/null 2>&1 || true

say "Caddy (web server s automatickým HTTPS)"
if ! command -v caddy >/dev/null; then
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi

say "Go (jen pro sestavení serveru)"
GOVER="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" != "$GOVER" ]; then
  curl -fsSL "https://go.dev/dl/${GOVER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=auto

say "Zdrojový kód ($BRANCH)"
id stinko >/dev/null 2>&1 || useradd --system --home "$BASE" --shell /usr/sbin/nologin stinko
mkdir -p "$BASE" "$BASE/pb_data" /var/backups/stinko
if [ -d "$BASE/src/.git" ]; then
  git -C "$BASE/src" fetch -q --depth 1 origin "$BRANCH" && git -C "$BASE/src" reset -q --hard FETCH_HEAD
else
  rm -rf "$BASE/src" && git clone -q --depth 1 -b "$BRANCH" "$REPO" "$BASE/src"
fi

say "Sestavení serveru (první sestavení trvá pár minut)"
(cd "$BASE/src/server" && CGO_ENABLED=0 go build -trimpath -o "$BASE/stinko.new" .)
rm -rf "$BASE/pb_public.new" && cp -r "$BASE/src/crm" "$BASE/pb_public.new"
systemctl stop stinko 2>/dev/null || true
mv "$BASE/stinko.new" "$BASE/stinko"
rm -rf "$BASE/pb_public" && mv "$BASE/pb_public.new" "$BASE/pb_public"
chown -R stinko:stinko "$BASE/pb_data"

cat > /etc/systemd/system/stinko.service <<EOF
[Unit]
Description=Stínko CRM
After=network-online.target
Wants=network-online.target

[Service]
User=stinko
Group=stinko
Environment=STINKO_PUBLIC=$BASE/pb_public
ExecStart=$BASE/stinko serve --http 127.0.0.1:8090 --dir $BASE/pb_data
Restart=always
RestartSec=3
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$BASE/pb_data

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable -q --now stinko

say "HTTPS pro $DOMAIN"
cat > /etc/caddy/Caddyfile <<EOF
$DOMAIN {
	encode zstd gzip
	# správa databáze PocketBase není z internetu dostupná
	@admin path /_/* /api/collections/_superusers/* /api/backups* /api/settings*
	respond @admin 404
	reverse_proxy 127.0.0.1:8090
	header {
		Strict-Transport-Security "max-age=31536000"
		X-Content-Type-Options nosniff
		Referrer-Policy same-origin
		-Server
	}
	# poptávkový formulář se vkládá do webu, vše ostatní vložit nejde
	@noembed not path /poptavka
	header @noembed X-Frame-Options DENY
}
EOF
BUSY="$(ss -ltnpH '( sport = :80 or sport = :443 )' | grep -v caddy || true)"
[ -z "$BUSY" ] || { echo "Porty 80/443 už používá jiný program, Caddy se nespustí:"; echo "$BUSY"; exit 1; }
systemctl enable -q caddy && systemctl restart caddy

say "Firewall (SSH, HTTP, HTTPS)"
ufw allow OpenSSH >/dev/null && ufw allow 80/tcp >/dev/null && ufw allow 443 >/dev/null
ufw --force enable >/dev/null

say "Noční zálohy (02:30)"
mkdir -p /etc/stinko && chmod 700 /etc/stinko
[ -f /etc/stinko/backup.env ] || cat > /etc/stinko/backup.env <<'EOF'
# Záloha mimo server (restic). Dokud není vyplněno, drží se jen 14 nočních kopií na tomto serveru.
# RESTIC_REPOSITORY=s3:https://...    (nebo sftp:uzivatel@server:/cesta)
# RESTIC_PASSWORD=...                 (heslo pro šifrování záloh – uschovejte ho mimo server!)
# AWS_ACCESS_KEY_ID=...
# AWS_SECRET_ACCESS_KEY=...
EOF
chmod 600 /etc/stinko/backup.env
cat > "$BASE/backup.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
set -a; . /etc/stinko/backup.env; set +a
D=/var/backups/stinko/\$(date +%F)
mkdir -p "\$D"
sqlite3 $BASE/pb_data/data.db ".backup '\$D/data.db'"
tar -C $BASE/pb_data -czf "\$D/storage.tgz" storage 2>/dev/null || true
ls -1d /var/backups/stinko/20* | head -n -14 | xargs -r rm -rf
if [ -n "\${RESTIC_REPOSITORY:-}" ]; then
  restic snapshots >/dev/null 2>&1 || restic init
  restic backup -q --tag stinko "\$D"
  restic forget -q --prune --keep-daily 14 --keep-weekly 8 --keep-monthly 12
fi
EOF
chmod 700 "$BASE/backup.sh"
cat > /etc/systemd/system/stinko-backup.service <<EOF
[Unit]
Description=Stínko CRM – záloha
[Service]
Type=oneshot
ExecStart=$BASE/backup.sh
EOF
cat > /etc/systemd/system/stinko-backup.timer <<'EOF'
[Unit]
Description=Stínko CRM – noční záloha
[Timer]
OnCalendar=*-*-* 02:30
Persistent=true
[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload && systemctl enable -q --now stinko-backup.timer

say "Kontrola"
sleep 2
systemctl is-active --quiet stinko && echo "CRM server: běží" || { echo "CRM server NEBĚŽÍ:"; journalctl -u stinko -n 30 --no-pager; exit 1; }
IP="$(curl -fsS -4 https://ifconfig.me 2>/dev/null || true)"
DNSIP="$(getent ahostsv4 "$DOMAIN" | awk 'NR==1{print $1}')"
if [ -n "$IP" ] && [ "$DNSIP" != "$IP" ]; then
  echo "POZOR: $DOMAIN zatím vede na '${DNSIP:-nic}', server má $IP. HTTPS se zapne samo, jakmile se DNS záznam projeví."
fi
echo
echo "Hotovo. Otevřete https://$DOMAIN a vytvořte účet administrátora."
