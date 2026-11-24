# UniFi's login as the lab plays it, for page.sh and lifecycle.sh. Measured on UniFi OS 6.0.4: the
# login is the cookie, a passed one is cached for 10 s, and the auth subrequest runs UniFi's own
# CSRF check. A write must carry the session's token in X-Csrf-Token, or nginx answers 403 and the
# daemon never sees it. nginx hands the token out as X-Csrf-Token on a read's answer, through the
# map below.

# The session's token, which the stand-in derives from the login cookie, so a new login has a new one.
unifi_login_py() {
  cat <<'PY'
import hashlib
def unifi_csrf(token):
    return "unifi-" + hashlib.sha256(token.encode()).hexdigest()[:16]
def unifi_auth(h):
    token = h.headers.get("X-Token", "")
    if token.startswith("lab-login"):
        expected, provided = unifi_csrf(token), h.headers.get("X-Provided-Csrf-Token", "")
        safe = h.headers.get("X-Original-Method", "") in ("GET", "HEAD", "OPTIONS")
        h.send_response(200 if expected == provided or safe else 403)
        h.send_header("X-Expected-Csrf-Token", expected)
        if provided:
            h.send_header("X-Provided-Csrf-Token", provided)
    else:
        h.send_response(401)
    h.send_header("Content-Length", "0")
    h.end_headers()
PY
}

unifi_csrf_map() {
  cat <<'CONF'
  map $sent_http_x_updated_csrf_token:$sent_http_x_provided_csrf_token:$sent_http_x_expected_csrf_token $csrf_token_header {
    default             '';
    ~^(.+):(.*):(.*)$   $sent_http_x_updated_csrf_token;
    ~^::(.+)$           $sent_http_x_expected_csrf_token;
  }
CONF
}

# unifi_auth_location LOCATION SOCKET: the auth subrequest, asking the stand-in on SOCKET, and
# the body UniFi answers a refused one with.
unifi_auth_location() {
  cat <<CONF
    location = $1 {
      internal;
      proxy_pass_request_headers off;
      proxy_pass_request_body off;
      proxy_set_header Content-Length "";
      proxy_set_header X-Original-Method \$request_method;
      proxy_set_header X-Token \$cookie_UNIFI_TOKEN;
      proxy_set_header X-Provided-Csrf-Token \$http_x_csrf_token;
      proxy_cache auth_responses;
      proxy_cache_key "\$cookie_UNIFI_TOKEN|\$http_x_csrf_token|\$request_method|\$remote_addr";
      proxy_cache_lock on;
      proxy_cache_valid 200 10s;
      proxy_ignore_headers Cache-Control Expires Set-Cookie;
      proxy_pass http://unix:$2:/auth;
    }
    error_page 403 = @unifi_forbidden;
    location @unifi_forbidden {
      default_type application/json;
      return 403 '{"error":{"code":403,"message":"Forbidden"}}';
    }
CONF
}

# unifi_auth_conf LOCATION: the include UniFi's apps use, ending as UniFi's auth.conf does.
unifi_auth_conf() {
  cat <<CONF
auth_request $1;
auth_request_set \$expected_csrf_token \$sent_http_x_expected_csrf_token;
proxy_set_header X-Expected-Csrf-Token \$expected_csrf_token;
auth_request_set \$provided_csrf_token \$sent_http_x_provided_csrf_token;
proxy_set_header X-Provided-Csrf-Token \$provided_csrf_token;
proxy_hide_header X-Csrf-Token;
add_header X-Csrf-Token \$csrf_token_header;
CONF
}

# unifi_token LOGIN URL: the token nginx hands out on a read's answer, as the page learns it.
unifi_token() {
  curl -s -m 10 -D - -o /dev/null -H "$1" "$2" | tr -d '\r' | sed -n 's/^[Xx]-[Cc]srf-[Tt]oken: *//p' | tail -1
}
