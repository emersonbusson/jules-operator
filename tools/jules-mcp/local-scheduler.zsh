#!/usr/bin/env zsh

set -eu
umask 077

script_path="${0:A}"
script_dir="${script_path:h}"
test_mode="${JULES_LOCAL_TEST_MODE:-false}"
quiet=false
ramshared_repo="${RAMSHARED_REPO:-}"
confirmation=""

usage() {
  print "Uso: tools/jules-mcp/local-scheduler.zsh start|run-once|watch-once|status|stop|promote|demote"
  print "  start      inicia 1 scheduler local e faz uma tentativa imediata"
  print "  run-once   executa 1 janela no processo atual"
  print "  watch-once reconcilia sessões sem abrir uma janela"
  print "  status     informa PID, última tentativa e resultado"
  print "  stop       encerra somente a instância local verificada"
  print "  promote    libera a cadência após confirmação humana 3/3"
  print "  demote     volta imediatamente ao gate de canário"
}

fail() {
  print -u2 -- "$1"
  return 2
}

parse_options() {
  while (( $# > 0 )); do
    case "$1" in
      --repo)
        (( $# >= 2 )) || {
          fail "Valor ausente para --repo."
          return $?
        }
        ramshared_repo="$2"
        shift 2
        ;;
      --quiet)
        quiet=true
        shift
        ;;
      --confirm)
        (( $# >= 2 )) || {
          fail "Valor ausente para --confirm."
          return $?
        }
        confirmation="$2"
        shift 2
        ;;
      *)
        fail "Opção desconhecida: $1"
        return $?
        ;;
    esac
  done
}

runtime_root() {
  if [[ "$test_mode" == "true" ]]; then
    local configured="${JULES_LOCAL_RUNTIME_ROOT:-}"
    [[ "$configured" == /* ]] || {
      fail "JULES_LOCAL_RUNTIME_ROOT precisa ser absoluto no modo de teste."
      return $?
    }
    print -r -- "$configured"
    return
  fi
  print -r -- "/run/user/${EUID}"
}

prepare_runtime() {
  local root="$1"
  if [[ "$test_mode" == "true" ]]; then
    command mkdir -p -m 700 -- "$root"
  fi
  [[ -d "$root" && -O "$root" && ! -L "$root" ]] || {
    fail "Runtime local ausente ou inseguro: $root"
    return $?
  }

  command mkdir -p -m 700 -- "$runtime_dir"
  [[ -d "$runtime_dir" && -O "$runtime_dir" && ! -L "$runtime_dir" ]] || {
    fail "Diretório do scheduler ausente ou inseguro."
    return $?
  }
  command chmod 700 -- "$runtime_dir"
}

resolve_ramshared_repo() {
  if [[ -z "$ramshared_repo" ]]; then
    ramshared_repo="$(
      command git -C "$PWD" rev-parse --show-toplevel 2>/dev/null
    )" || true
  fi
  [[ -n "$ramshared_repo" ]] || {
    fail "Informe o checkout RamShared com --repo /caminho/ramshared."
    return $?
  }
  ramshared_repo="${ramshared_repo:A}"

  if [[ "$test_mode" == "true" ]]; then
    return
  fi

  [[ -d "$ramshared_repo/.git" || -f "$ramshared_repo/.git" ]] || {
    fail "Checkout Git inválido: $ramshared_repo"
    return $?
  }
  local remote
  remote="$(command git -C "$ramshared_repo" remote get-url origin 2>/dev/null)" ||
    {
      fail "O checkout não possui remote origin."
      return $?
    }
  case "$remote" in
    https://github.com/emersonbusson/ramshared | https://github.com/emersonbusson/ramshared.git | \
      git@github.com:emersonbusson/ramshared | git@github.com:emersonbusson/ramshared.git | \
      ssh://git@github.com/emersonbusson/ramshared | \
      ssh://git@github.com/emersonbusson/ramshared.git)
      ;;
    *)
      fail "Remote recusado; esperado emersonbusson/ramshared."
      return $?
      ;;
  esac
}

read_pid() {
  [[ -f "$pid_file" && -O "$pid_file" && ! -L "$pid_file" ]] || return 1
  local pid
  pid="$(<"$pid_file")"
  [[ "$pid" == <-> ]] || return 1
  print -r -- "$pid"
}

verified_pid() {
  local pid
  pid="$(read_pid)" || return 1
  command kill -0 "$pid" 2>/dev/null || return 1
  [[ -r "/proc/${pid}/cmdline" ]] || return 1
  local command_line
  command_line="$(command tr '\0' ' ' <"/proc/${pid}/cmdline")"
  [[ "$command_line" == *"$script_path"*"_loop"* ]] || return 1
  print -r -- "$pid"
}

write_status() {
  local exit_code="$1"
  local operation="${2:-dispatch}"
  local temporary
  temporary="$(command mktemp "${runtime_dir}/.status.XXXXXX")"
  print -r -- "attempted_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$temporary"
  print -r -- "exit_code=${exit_code}" >>"$temporary"
  print -r -- "operation=${operation}" >>"$temporary"
  command chmod 600 -- "$temporary"
  command mv -f -- "$temporary" "$status_file"
}

refresh_main() {
  local git_runner="git"
  if [[ "$test_mode" == "true" ]]; then
    [[ -n "${JULES_LOCAL_GIT:-}" ]] || return 0
    git_runner="$JULES_LOCAL_GIT"
  fi
  if ! "$git_runner" -C "$ramshared_repo" fetch --quiet origin main; then
    fail "Falha ao atualizar origin/main; nenhuma chamada Jules foi feita."
    return $?
  fi
}

run_once() {
  (
  local runner="${JULES_LOCAL_RUNNER:-${script_dir}/jules.zsh}"
  [[ -x "$runner" ]] || {
    fail "Runner local ausente ou sem permissão: $runner"
    return $?
  }
  if ! command mkdir -- "$lock_dir" 2>/dev/null; then
    [[ "$quiet" == "true" ]] ||
      print "Janela ignorada: outra execução local está ativa."
    return 0
  fi
  trap 'command rmdir -- "$lock_dir" 2>/dev/null || true' EXIT HUP INT TERM

  local exit_code=0
  refresh_main || {
    exit_code=$?
    write_status "$exit_code" "dispatch"
    command rmdir -- "$lock_dir" 2>/dev/null || true
    trap - EXIT HUP INT TERM
    return "$exit_code"
  }

  local promoted=false
  [[ -f "$promotion_file" && -O "$promotion_file" &&
    ! -L "$promotion_file" ]] && promoted=true

  (
    cd "$ramshared_repo"
    command env -u JULES_API_KEY \
      RAMSHARED_REPO="$ramshared_repo" \
      JULES_DAILY_AUTO_PR_ENABLED=true \
      JULES_DAILY_CANARY_PROMOTED="$promoted" \
      "$runner" daily-run
  ) || exit_code=$?
  write_status "$exit_code" "dispatch"
  command rmdir -- "$lock_dir" 2>/dev/null || true
  trap - EXIT HUP INT TERM
  return "$exit_code"
  )
}

run_watch() {
  (
  local runner="${JULES_LOCAL_RUNNER:-${script_dir}/jules.zsh}"
  [[ -x "$runner" ]] || {
    fail "Runner local ausente ou sem permissão: $runner"
    return $?
  }
  if ! command mkdir -- "$lock_dir" 2>/dev/null; then
    [[ "$quiet" == "true" ]] ||
      print "Observação ignorada: outra execução local está ativa."
    return 0
  fi
  trap 'command rmdir -- "$lock_dir" 2>/dev/null || true' EXIT HUP INT TERM

  local exit_code=0
  (
    cd "$ramshared_repo"
    command env -u JULES_API_KEY \
      RAMSHARED_REPO="$ramshared_repo" \
      "$runner" daily-watch
  ) || exit_code=$?
  write_status "$exit_code" "watch"
  command rmdir -- "$lock_dir" 2>/dev/null || true
  trap - EXIT HUP INT TERM
  return "$exit_code"
  )
}

start_scheduler() {
  local active_pid
  if active_pid="$(verified_pid)"; then
    [[ "$quiet" == "true" ]] ||
      print "Scheduler Jules local já está ativo no PID $active_pid."
    return 0
  fi
  command rm -f -- "$pid_file"

  if [[ -e "$log_file" || -L "$log_file" ]]; then
    [[ -f "$log_file" && -O "$log_file" && ! -L "$log_file" ]] || {
      fail "Log do scheduler é inseguro; esperado arquivo regular do usuário."
      return $?
    }
    if (( $(command stat -c '%s' "$log_file" 2>/dev/null || print 0) > 1048576 )); then
      command mv -f -- "$log_file" "${log_file}.previous"
    fi
  fi
  : >>"$log_file"
  command chmod 600 -- "$log_file"

  local runner="${JULES_LOCAL_RUNNER:-${script_dir}/jules.zsh}"
  local interval="${JULES_LOCAL_INTERVAL_SECONDS:-3600}"
  local detach=()
  if [[ "$test_mode" != "true" ]]; then
    interval=3600
    runner="${script_dir}/jules.zsh"
    command -v setsid >/dev/null || {
      fail "setsid não encontrado; scheduler local não pode ser desacoplado."
      return $?
    }
    detach=(setsid)
  fi
  [[ "$interval" == <-> && "$interval" -ge 60 ]] || {
    fail "Intervalo local inválido; mínimo de 60 segundos."
    return $?
  }

  command env -u JULES_API_KEY \
    RAMSHARED_REPO="$ramshared_repo" \
    JULES_LOCAL_INTERVAL_SECONDS="$interval" \
    JULES_LOCAL_RUNNER="$runner" \
    JULES_LOCAL_RUNTIME_ROOT="${JULES_LOCAL_RUNTIME_ROOT:-}" \
    JULES_LOCAL_TEST_EVIDENCE="${JULES_LOCAL_TEST_EVIDENCE:-}" \
    JULES_LOCAL_TEST_MODE="$test_mode" \
    nohup "${detach[@]}" zsh "$script_path" _loop --repo "$ramshared_repo" \
      >>"$log_file" 2>&1 </dev/null &
  local scheduler_pid=$!
  print -r -- "$scheduler_pid" >"$pid_file"
  command chmod 600 -- "$pid_file"

  local attempts=0
  while (( attempts < 20 )); do
    if verified_pid >/dev/null; then
      [[ "$quiet" == "true" ]] ||
        print "Scheduler Jules local iniciado no PID $scheduler_pid."
      return 0
    fi
    command sleep 0.05
    (( attempts += 1 ))
  done
  command rm -f -- "$pid_file"
  fail "O processo local encerrou antes de ficar pronto."
}

show_status() {
  if [[ -f "$promotion_file" && -O "$promotion_file" &&
    ! -L "$promotion_file" ]]; then
    print "Canário: promovido."
  else
    print "Canário: bloqueado até confirmação 3/3."
  fi
  local pid
  if ! pid="$(verified_pid)"; then
    print "Scheduler Jules local inativo."
    return 1
  fi
  print "Scheduler Jules local ativo no PID $pid."
  if [[ -f "$status_file" && -O "$status_file" && ! -L "$status_file" ]]; then
    command sed -n 's/^attempted_at=/Última tentativa: /p; s/^exit_code=/Resultado: /p; s/^operation=/Operação: /p' \
      "$status_file"
  else
    print "Primeira tentativa ainda sem resultado."
  fi
}

stop_scheduler() {
  local pid
  if ! pid="$(verified_pid)"; then
    command rm -f -- "$pid_file"
    print "Scheduler Jules local já estava inativo."
    return 0
  fi
  local session_id
  session_id="$(command ps -o sid= -p "$pid" | command tr -d ' ')"
  if [[ "$session_id" == "$pid" ]]; then
    command kill -TERM -- "-$pid" 2>/dev/null || true
  else
    local children=""
    [[ -r "/proc/${pid}/task/${pid}/children" ]] &&
      children="$(<"/proc/${pid}/task/${pid}/children")"
    local child
    for child in ${(z)children}; do
      [[ "$child" == <-> ]] && command kill -TERM "$child" 2>/dev/null || true
    done
    command kill -TERM "$pid" 2>/dev/null || true
  fi
  local attempts=0
  while verified_pid >/dev/null && (( attempts < 40 )); do
    command sleep 0.05
    (( attempts += 1 ))
  done
  if verified_pid >/dev/null; then
    fail "PID $pid não encerrou no prazo; nenhuma força destrutiva foi usada."
    return $?
  fi
  command rm -f -- "$pid_file"
  print "Scheduler Jules local encerrado."
}

promote_scheduler() {
  [[ "$confirmation" == "3/3" ]] || {
    fail "Promoção recusada. Use --confirm 3/3 após auditar o canário."
    return $?
  }
  local temporary
  temporary="$(command mktemp "${runtime_dir}/.promotion.XXXXXX")"
  print -r -- "promoted_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$temporary"
  command chmod 600 -- "$temporary"
  command mv -f -- "$temporary" "$promotion_file"
  print "Canário promovido; próximas janelas podem usar a cadência horária."
}

demote_scheduler() {
  command rm -f -- "$promotion_file"
  print "Canário despromovido; novas janelas voltaram ao gate inicial."
}

command_name="${1:-}"
(( $# > 0 )) && shift
parse_options "$@"
root="$(runtime_root)"
runtime_dir="${root}/ramshared/jules-operator"
pid_file="${runtime_dir}/scheduler.pid"
status_file="${runtime_dir}/status"
log_file="${runtime_dir}/scheduler.log"
lock_dir="${runtime_dir}/run.lock"
promotion_file="${runtime_dir}/canary-promoted"
prepare_runtime "$root"

case "$command_name" in
  start)
    resolve_ramshared_repo
    start_scheduler
    ;;
  run-once)
    resolve_ramshared_repo
    run_once
    ;;
  watch-once)
    resolve_ramshared_repo
    run_watch
    ;;
  status)
    show_status
    ;;
  stop)
    stop_scheduler
    ;;
  promote)
    promote_scheduler
    ;;
  demote)
    demote_scheduler
    ;;
  _loop)
    resolve_ramshared_repo
    trap 'exit 0' HUP INT TERM
    dispatch_interval="${JULES_LOCAL_INTERVAL_SECONDS:-3600}"
    watch_interval=300
    elapsed="$dispatch_interval"
    while true; do
      if (( elapsed >= dispatch_interval )); then
        run_once || true
        elapsed=0
      else
        run_watch || true
      fi
      command sleep "$watch_interval"
      (( elapsed += watch_interval ))
    done
    ;;
  *)
    usage
    exit 2
    ;;
esac
