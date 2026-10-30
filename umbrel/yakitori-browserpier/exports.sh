# Read on the host, where BrowserPier can't see from its container.

# The addresses live viewers reach this Umbrel at: the LAN's and
# Tailscale's, not Docker's own bridges.
browserpier_ips="$({ ip -o -4 addr show scope global | awk '$2 !~ /^(docker|br-|veth)/ { split($4, a, "/"); print a[1] }' | paste -sd, -; } 2>/dev/null)" || browserpier_ips=""
export APP_YAKITORI_BROWSERPIER_WEBRTC_IPS="${browserpier_ips}"

# The Docker socket's group, so BrowserPier can start browsers.
export APP_YAKITORI_BROWSERPIER_DOCKER_GID="$(stat -c %g /var/run/docker.sock 2>/dev/null || echo 999)"
