#!/usr/bin/env zsh

set -eu
umask 077

script_dir="${0:A:h}"
repo_dir="${script_dir:h:h}"
artifact_dir="${repo_dir}/artifacts/jules"
manifest="${script_dir}/session-plan.json"
ramshared_repo="${RAMSHARED_REPO:-$PWD}"
source "${script_dir}/runtime-key.zsh"

usage() {
  print "Uso: tools/jules-mcp/jules.zsh check|inventory|launch|status|measure|cleanup|audit-pr|finalize-wave|daily-run|daily-watch"
  print "  check      valida dependências, chave e manifesto sem mutação"
  print "  inventory  salva a árvore atual em artifacts/jules/"
  print "  launch     cria o lote após confirmação exata"
  print "  status     consulta o último lote criado"
  print "  measure    mede aderência do plano em 10 requisitos"
  print "  cleanup    apaga o inventário escolhido após confirmação exata"
  print "  audit-pr   valida a auditoria 30/30 de um PR de intake"
  print "  finalize-wave fecha PRs superseded após CI verde do consolidado"
  print "  daily-run  gera e despacha a janela horária do portfólio Pro"
  print "  daily-watch reconcilia estados e métricas sem criar sessões"
}

require_key() {
  if ! load_jules_runtime_key; then
    print -u2 "Chave ausente. Execute uma vez: tools/jules-mcp/setup-local.zsh"
    return 2
  fi
}

latest_artifact() {
  local pattern="$1"
  local matches=("${artifact_dir}"/${~pattern}(Nom))
  if (( ${#matches} == 0 )); then
    return 1
  fi
  print -r -- "${matches[1]}"
}

command="${1:-}"
case "$command" in
  check)
    require_key
    command -v go >/dev/null || {
      print -u2 "Go não encontrado."
      exit 2
    }
    (
      cd "$script_dir"
      GOWORK=off go test ./...
    )
    print -r -- "x-goog-api-key: ${JULES_API_KEY}" |
      curl --silent --show-error --fail --max-time 20 \
        --output /dev/null \
        --header @- \
        "https://jules.googleapis.com/v1alpha/sessions?pageSize=1"
    print "Pronto: testes locais e API Jules passaram."
    ;;
  inventory)
    require_key
    mkdir -p -m 700 "$artifact_dir"
    output="${artifact_dir}/inventory-$(date -u +%Y%m%dT%H%M%SZ).json"
    (
      cd "$script_dir"
      GOWORK=off go run . batch inventory --output "$output"
    )
    print "Inventário salvo em: $output"
    ;;
  launch)
    require_key
    count="$(node -e 'const m=require(process.argv[1]); process.stdout.write(String(m.sessions.length))' "$manifest")"
    print "Manifesto: $manifest"
    print "Sessões a criar: $count"
    print -n "Digite $count para confirmar ou Esc para cancelar: "
    read -r confirmation
    [[ "$confirmation" == "$count" ]] || {
      print "Cancelado; nenhuma sessão foi criada."
      exit 2
    }
    mkdir -p -m 700 "$artifact_dir"
    output="${artifact_dir}/launch-$(date -u +%Y%m%dT%H%M%SZ).json"
    (
      cd "$script_dir"
      JULES_MAX_BATCH_SESSIONS="$count" GOWORK=off go run . batch launch \
        --manifest "$manifest" --output "$output" --confirm-count "$count"
    )
    print "Lote criado e registrado em: $output"
    print "Planos continuam pendentes; use status e revise antes de aprovar."
    ;;
  status)
    require_key
    launch="$(latest_artifact 'launch-*.json')" || {
      print -u2 "Nenhum artefato de launch encontrado."
      exit 2
    }
    output="${artifact_dir}/status-$(date -u +%Y%m%dT%H%M%SZ).json"
    (
      cd "$script_dir"
      GOWORK=off go run . batch status --launch "$launch" --output "$output"
    )
    print "Status salvo em: $output"
    ;;
  measure)
    require_key
    launch="$(latest_artifact 'launch-*.json')" || {
      print -u2 "Nenhum artefato de launch encontrado."
      exit 2
    }
    output="${artifact_dir}/metrics-$(date -u +%Y%m%dT%H%M%SZ).json"
    (
      cd "$script_dir"
      GOWORK=off go run . batch measure --launch "$launch" --output "$output"
    )
    print "Métricas salvas em: $output"
    ;;
  cleanup)
    require_key
    inventory="$(latest_artifact 'inventory-*.json')" || {
      print -u2 "Nenhum inventário encontrado. Execute inventory primeiro."
      exit 2
    }
    count="$(node -e 'const a=require(process.argv[1]); process.stdout.write(String(a.sessions.length))' "$inventory")"
    print "ATENÇÃO: exclusão irreversível de $count sessões listadas em $inventory"
    print -n "Digite $count para confirmar ou Esc para cancelar: "
    read -r confirmation
    [[ "$confirmation" == "$count" ]] || {
      print "Cancelado; nenhuma sessão foi excluída."
      exit 2
    }
    (
      cd "$script_dir"
      GOWORK=off go run . batch cleanup --inventory "$inventory" --confirm-count "$count"
    )
    print "Limpeza concluída e verificada: 0 sessões restantes."
    ;;
  audit-pr)
    require_key
    audit_path="${2:-}"
    allowed_actor="${JULES_GITHUB_ACTOR:-}"
    [[ -n "$audit_path" ]] || {
      print -u2 "Informe o caminho do artefato JSON da auditoria."
      exit 2
    }
    audit_path="${audit_path:A}"
    [[ -n "$allowed_actor" ]] || {
      print -u2 "JULES_GITHUB_ACTOR ausente; use o autor medido no canário."
      exit 2
    }
    (
      cd "$script_dir"
      GOWORK=off go run . batch daily-pr-audit-live \
        --input "$audit_path" \
        --allowed-actor "$allowed_actor"
    )
    print "Auditoria válida: 30/30 lentes, 0 Actions e 0 merge."
    ;;
  finalize-wave)
    require_key
    manifest_path="${2:-}"
    confirmed_pr="${3:-}"
    allowed_actor="${JULES_GITHUB_ACTOR:-}"
    [[ -f "$manifest_path" ]] || {
      print -u2 "Informe o manifesto JSON de finalização existente."
      exit 2
    }
    manifest_path="${manifest_path:A}"
    [[ "$confirmed_pr" == <-> ]] || {
      print -u2 "Confirme o número exato do PR humano consolidado."
      exit 2
    }
    [[ -n "$allowed_actor" ]] || {
      print -u2 "JULES_GITHUB_ACTOR ausente; use o autor medido no canário."
      exit 2
    }
    (
      cd "$script_dir"
      GOWORK=off go run . batch daily-finalize \
        --input "$manifest_path" \
        --output-dir "${artifact_dir}/finalizations" \
        --allowed-actor "$allowed_actor" \
        --confirm-consolidated-pr "$confirmed_pr"
    )
    print "Onda finalizada: PRs Jules fechados sem merge e recibos gravados."
    ;;
  daily-run)
    require_key
    command -v gh >/dev/null || {
      print -u2 "GitHub CLI não encontrado."
      exit 2
    }
    command -v go >/dev/null || {
      print -u2 "Go não encontrado."
      exit 2
    }
    mkdir -p -m 700 "$artifact_dir"
    local_day="$(TZ=America/Sao_Paulo date +%F)"
    local_hour="$(TZ=America/Sao_Paulo date +%H)"
    local_hour="$((10#${local_hour}))"
    issues="${artifact_dir}/daily-issues-${local_day}.json"
    issue_inventory_json="${artifact_dir}/daily-issue-inventory-${local_day}.json"
    issue_inventory_markdown="${artifact_dir}/daily-issue-inventory-${local_day}.md"
    daily_manifest="${artifact_dir}/daily-manifest-${local_day}.json"
    open_prs="${artifact_dir}/daily-open-prs-$(date -u +%Y%m%dT%H%M%SZ).json"
    dispatch="${artifact_dir}/daily-dispatch-$(date -u +%Y%m%dT%H%M%SZ).json"
    reconcile="${artifact_dir}/daily-reconcile-$(date -u +%Y%m%dT%H%M%SZ).json"
    metrics="${artifact_dir}/daily-metrics-$(date -u +%Y%m%dT%H%M%SZ).json"
    archive="${artifact_dir}/daily-archive-$(date -u +%Y%m%dT%H%M%SZ)"
    finalizations="${artifact_dir}/finalizations"
    implementation_only=()
    if [[ -n "${JULES_DAILY_IMMEDIATE_BATCH_SIZE:-}" ]]; then
      implementation_only=(--implementation-only)
    fi

    [[ -d "$ramshared_repo/.git" || -f "$ramshared_repo/.git" ]] || {
      print -u2 "Checkout RamShared inválido: $ramshared_repo"
      exit 2
    }
    git -C "$ramshared_repo" fetch --quiet origin \
      refs/heads/main:refs/remotes/origin/main \
      refs/heads/jules/inbox:refs/remotes/origin/jules/inbox
    gh issue list --repo emersonbusson/ramshared --state open --limit 200 \
      --json number,title,body,labels,updatedAt,url >"$issues"
    gh pr list --repo emersonbusson/ramshared --base jules/inbox --state open --limit 301 \
      --json number,url,baseRefName,state >"$open_prs"
    open_pr_count="$(
      gh pr list --repo emersonbusson/ramshared --base jules/inbox --state open \
        --limit 301 --json number --jq 'length'
    )"
    base_sha="$(git -C "$ramshared_repo" rev-parse origin/main)"
    inbox_sha="$(git -C "$ramshared_repo" rev-parse origin/jules/inbox)"
    if [[ "$inbox_sha" != "$base_sha" ]]; then
      (( open_pr_count == 0 )) || {
        print -u2 "jules/inbox defasada com $open_pr_count PR(s) aberto(s); dispatch bloqueado."
        exit 2
      }
      git -C "$ramshared_repo" merge-base --is-ancestor "$inbox_sha" "$base_sha" || {
        print -u2 "jules/inbox divergiu de main; fast-forward automático recusado."
        exit 2
      }
      git -C "$ramshared_repo" push origin "${base_sha}:refs/heads/jules/inbox"
      git -C "$ramshared_repo" fetch --quiet origin \
        refs/heads/jules/inbox:refs/remotes/origin/jules/inbox
      inbox_sha="$(git -C "$ramshared_repo" rev-parse origin/jules/inbox)"
      [[ "$inbox_sha" == "$base_sha" ]] || {
        print -u2 "Fast-forward de jules/inbox não foi comprovado."
        exit 2
      }
    fi
    (
      cd "$script_dir"
      GOWORK=off go run . batch daily-issue-inventory \
        --issues "$issues" \
        --json-output "$issue_inventory_json" \
        --markdown-output "$issue_inventory_markdown"
      GOWORK=off go run . batch daily-plan \
        --issues "$issues" \
        --output "$daily_manifest" \
        --base-sha "$base_sha" \
        --day "$local_day" \
        --target 100 \
        "${implementation_only[@]}" \
        --finalization-dir "$finalizations"
      GOWORK=off go run . batch daily-reconcile \
        --output "$reconcile" \
        --confirm-prefix "[RS-EXAMPLE "
      GOWORK=off go run . batch daily-metrics \
        --output "$metrics" \
        --finalization-dir "$finalizations" \
        --audit-dir "$artifact_dir"
      GOWORK=off go run . batch daily-reap \
        --output-dir "$archive" \
        --finalization-dir "$finalizations" \
        --confirm-prefix "[RS-EXAMPLE "
    )

    promoted=()
    if [[ "${JULES_DAILY_CANARY_PROMOTED:-false}" == "true" ]]; then
      promoted=(--promoted)
    fi
    auto_pr=()
    if [[ "${JULES_DAILY_AUTO_PR_ENABLED:-false}" == "true" ]]; then
      auto_pr=(--auto-pr-enabled)
    fi
    retry_canary=()
    if [[ "${JULES_DAILY_RETRY_CANARY:-false}" == "true" ]]; then
      retry_canary=(--retry-canary)
    fi
    immediate_batch=()
    if [[ -n "${JULES_DAILY_IMMEDIATE_BATCH_SIZE:-}" ]]; then
      immediate_batch=(--immediate-batch-size "$JULES_DAILY_IMMEDIATE_BATCH_SIZE")
    fi
    (
      cd "$script_dir"
      GOWORK=off go run . batch daily-dispatch \
        --manifest "$daily_manifest" \
        --output "$dispatch" \
        --open-prs "$open_prs" \
        --confirm-target 100 \
        --hour "$local_hour" \
        --inbox-sha "$inbox_sha" \
        --paused-ok \
        "${promoted[@]}" \
        "${retry_canary[@]}" \
        "${immediate_batch[@]}" \
        "${auto_pr[@]}"
    )
    print "Janela ${local_day}T${local_hour}:00 concluída."
    print "Manifesto: $daily_manifest"
    print "Inventário de issues: $issue_inventory_markdown"
    print "Dispatch: $dispatch"
    print "Reconciliação: $reconcile"
    print "Métricas: $metrics"
    print "Evidências de sessões removidas: $archive"
    ;;
  daily-watch)
    require_key
    command -v go >/dev/null || {
      print -u2 "Go não encontrado."
      exit 2
    }
    mkdir -p -m 700 "$artifact_dir"
    timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
    reconcile="${artifact_dir}/daily-reconcile-${timestamp}.json"
    metrics="${artifact_dir}/daily-metrics-${timestamp}.json"
    finalizations="${artifact_dir}/finalizations"
    (
      cd "$script_dir"
      GOWORK=off go run . batch daily-reconcile \
        --output "$reconcile" \
        --confirm-prefix "[RS-EXAMPLE "
      GOWORK=off go run . batch daily-metrics \
        --output "$metrics" \
        --finalization-dir "$finalizations" \
        --audit-dir "$artifact_dir"
    )
    print "Observação periódica concluída sem abrir nova janela."
    print "Reconciliação: $reconcile"
    print "Métricas: $metrics"
    ;;
  *)
    usage
    exit 2
    ;;
esac
