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

# Start fail2ban
[ "$XUI_ENABLE_FAIL2BAN" = "true" ] && fail2ban-client -x start

# Run x-ui
exec /app/x-ui
