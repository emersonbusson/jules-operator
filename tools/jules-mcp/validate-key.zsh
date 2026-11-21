#!/usr/bin/env zsh

validate_jules_key() {
  if [[ -z "${JULES_API_KEY:-}" ]]; then
    print -u2 "Chave Jules ausente."
    return 2
  fi

  print "Validando a chave na API oficial do Jules..."
  local sources_http
  local sessions_http
  local sources_curl_status
  local sessions_curl_status

  sources_http=$(
    print -r -- "x-goog-api-key: $JULES_API_KEY" |
      curl --silent --show-error --max-time 20 \
        --output /dev/null \
        --write-out "%{http_code}" \
        --header @- \
        "https://jules.googleapis.com/v1alpha/sources"
  )
  sources_curl_status=$?

  sessions_http=$(
    print -r -- "x-goog-api-key: $JULES_API_KEY" |
      curl --silent --show-error --max-time 20 \
        --output /dev/null \
        --write-out "%{http_code}" \
        --header @- \
        "https://jules.googleapis.com/v1alpha/sessions?pageSize=1"
  )
  sessions_curl_status=$?

  if (( sources_curl_status != 0 || sessions_curl_status != 0 )); then
    print -u2 "Falha de rede ao validar a chave."
    return 2
  fi

  if [[ "$sources_http" != "200" || "$sessions_http" != "200" ]]; then
    print -u2 \
      "Chave recusada: sources HTTP $sources_http; sessions HTTP $sessions_http."
    return 2
  fi

  print "Chave Jules válida: sources HTTP 200; sessions HTTP 200."
}
