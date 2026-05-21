#!/bin/sh

HYSTERIA_CERT_DIR="${XUI_HYSTERIA_CERT_DIR:-/root/cert/hysteria2}"
HYSTERIA_CERT_FILE="${XUI_HYSTERIA_CERT_FILE:-$HYSTERIA_CERT_DIR/self.crt}"
HYSTERIA_KEY_FILE="${XUI_HYSTERIA_KEY_FILE:-$HYSTERIA_CERT_DIR/self.key}"

if [ ! -s "$HYSTERIA_CERT_FILE" ] || [ ! -s "$HYSTERIA_KEY_FILE" ]; then
  mkdir -p "$HYSTERIA_CERT_DIR"
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 3650 \
    -subj "/CN=3x-ui-hysteria2" \
    -keyout "$HYSTERIA_KEY_FILE" \
    -out "$HYSTERIA_CERT_FILE" >/dev/null 2>&1
  chmod 600 "$HYSTERIA_KEY_FILE"
fi

configure_fail2ban() {
  mkdir -p /var/log/x-ui /etc/fail2ban/filter.d /etc/fail2ban/action.d /etc/fail2ban/jail.d
  touch /var/log/x-ui/3xipl.log /var/log/x-ui/3xipl-banned.log

  cat >/etc/fail2ban/filter.d/3x-ipl.conf <<'EOF'
[Definition]
datepattern = ^%%Y/%%m/%%d %%H:%%M:%%S
failregex = \[LIMIT_IP\]\s*Email\s*=\s*<F-USER>.+</F-USER>\s*\|\|\s*Disconnecting OLD IP\s*=\s*<ADDR>\s*\|\|\s*Timestamp\s*=\s*\d+
ignoreregex =
EOF

  cat >/etc/fail2ban/action.d/3x-ipl.conf <<'EOF'
[Definition]
actionstart = iptables -N f2b-<name>
              iptables -A f2b-<name> -j RETURN
              iptables -I INPUT -j f2b-<name>
actionstop = iptables -D INPUT -j f2b-<name>
             iptables -F f2b-<name>
             iptables -X f2b-<name>
actioncheck = iptables -n -L INPUT | grep -q 'f2b-<name>[ \t]'
actionban = iptables -I f2b-<name> 1 -s <ip> -j DROP
            echo "$(date +"%%Y/%%m/%%d %%H:%%M:%%S")   BAN   [Email] = <F-USER> [IP] = <ip> banned for <bantime> seconds." >> /var/log/x-ui/3xipl-banned.log
actionunban = iptables -D f2b-<name> -s <ip> -j DROP
              echo "$(date +"%%Y/%%m/%%d %%H:%%M:%%S")   UNBAN   [Email] = <F-USER> [IP] = <ip> unbanned." >> /var/log/x-ui/3xipl-banned.log

[Init]
name = default
EOF

  cat >/etc/fail2ban/jail.d/3x-ipl.conf <<'EOF'
[3x-ipl]
enabled = true
backend = polling
filter = 3x-ipl
action = 3x-ipl
logpath = /var/log/x-ui/3xipl.log
maxretry = 1
findtime = 60
bantime = 30m
EOF
}

# Start fail2ban
if [ "$XUI_ENABLE_FAIL2BAN" = "true" ]; then
  configure_fail2ban
  fail2ban-client -x start
fi

# Run x-ui
exec /app/x-ui
