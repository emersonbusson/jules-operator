#!/usr/bin/env zsh

# Carrega a credencial da memória do processo, do runtime volátil ou do
# chaveiro de login. O valor nunca é impresso.
load_jules_runtime_key() {
  if [[ -n "${JULES_API_KEY:-}" ]]; then
    return 0
  fi

  local runtime_root="/run/user/${EUID}"
  if [[ "${JULES_LOCAL_TEST_MODE:-false}" == "true" ]]; then
    runtime_root="${JULES_LOCAL_RUNTIME_ROOT:-}"
    [[ "$runtime_root" == /* ]] || return 2
  fi
  local runtime_dir="${runtime_root}/ramshared"
  local runtime_key_file="${runtime_dir}/jules-api-key"
  local runtime_dir_mode=""
  local runtime_key_mode=""
  if [[ -d "$runtime_dir" && -O "$runtime_dir" && ! -L "$runtime_dir" ]]; then
    runtime_dir_mode="$(command stat -c '%a' "$runtime_dir" 2>/dev/null)"
  fi
  if [[ "$runtime_dir_mode" == "700" &&
    -f "$runtime_key_file" &&
    -O "$runtime_key_file" &&
    ! -L "$runtime_key_file" ]]; then
    runtime_key_mode="$(command stat -c '%a' "$runtime_key_file" 2>/dev/null)"
  fi
  if [[ "$runtime_key_mode" == "600" ]]; then
    JULES_API_KEY="$(<"$runtime_key_file")"
    if [[ -n "$JULES_API_KEY" ]]; then
      export JULES_API_KEY
      unset runtime_dir runtime_dir_mode runtime_key_file runtime_key_mode
      return 0
    fi
  fi

  local credential_helper="${JULES_CREDENTIAL_HELPER:-${script_dir:-}/credential-store.py}"
  [[ -x "$credential_helper" && -O "$credential_helper" && ! -L "$credential_helper" ]] ||
    return 2
  local stored_key
  stored_key="$("$credential_helper" read 2>/dev/null)" || return 2
  [[ -n "$stored_key" ]] || return 2
  export JULES_API_KEY="$stored_key"
  unset stored_key credential_helper runtime_root runtime_dir runtime_dir_mode
  unset runtime_key_file runtime_key_mode
}
