import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

const script = (name) =>
  readFileSync(
    new URL(`../../tools/jules-mcp/${name}`, import.meta.url),
    'utf8'
  )

test('Jules key never enters curl process arguments', () => {
  for (const name of ['validate-key.zsh', 'jules.zsh']) {
    const source = script(name)
    assert.doesNotMatch(
      source,
      /--header\s+["']?x-goog-api-key:\s*\$\{?JULES_API_KEY/
    )
    assert.match(source, /--header @-/)
  }
})

test('Jules runtime helpers never print the key value', () => {
  for (const name of [
    'load-key.zsh',
    'runtime-key.zsh',
    'share-key.zsh',
    'mcp-server.zsh',
    'setup-local.zsh',
    'validate-key.zsh',
  ]) {
    const source = script(name)
    const keyWrites = source
      .split('\n')
      .filter(
        (line) =>
          /(?:print|echo)/.test(line) &&
          /(?:\$JULES_API_KEY|\$\{JULES_API_KEY\})/.test(line)
      )
    for (const line of keyWrites) {
      assert.match(
        line,
        /(?:>"\$runtime_(?:key|temp)_file"|\||\| "\$credential_helper" write)$/
      )
    }
  }
})

test('Jules runtime file rejects symlinks and is published atomically', () => {
  const loader = script('runtime-key.zsh')
  const publisher = script('share-key.zsh')

  assert.match(loader, /! -L "\$runtime_dir"/)
  assert.match(loader, /! -L "\$runtime_key_file"/)
  assert.match(publisher, /command mktemp/)
  assert.match(publisher, /command mv -f/)
  assert.match(publisher, /\|\| -L "\$runtime_root"/)
  assert.match(publisher, /\|\| -L "\$runtime_dir"/)
})

test('Jules persistent key uses the desktop keyring through stdin', () => {
  const helper = script('credential-store.py')
  const setup = script('setup-local.zsh')

  assert.match(helper, /Secret\.password_store_sync/)
  assert.match(helper, /Secret\.password_lookup_sync/)
  assert.match(helper, /sys\.stdin\.read\(\)/)
  assert.doesNotMatch(helper, /sys\.argv\[[2-9]\]/)
  assert.doesNotMatch(helper, /open\([^\n]*jules-api-key/)
  assert.match(
    setup,
    /env -u JULES_API_KEY "\$credential_helper" write/
  )
  assert.match(
    setup,
    /"\$publisher"\nunset JULES_API_KEY\n"\$hook_installer" install/
  )
})

test('Jules runtime reloads the persistent key without printing it', () => {
  const directory = mkdtempSync(join(tmpdir(), 'jules-keyring-test-'))
  try {
    const helperPath = join(directory, 'fake-credential-store.zsh')
    writeFileSync(
      helperPath,
      [
        '#!/usr/bin/env zsh',
        '[[ "$1" == read ]] || exit 2',
        "print -rn -- 'persistent-test-key'",
        '',
      ].join('\n')
    )
    chmodSync(helperPath, 0o700)

    const runtimePath = new URL(
      '../../tools/jules-mcp/runtime-key.zsh',
      import.meta.url
    )
    const result = spawnSync(
      'zsh',
      [
        '-c',
        'source "$1"; load_jules_runtime_key; [[ "$JULES_API_KEY" == persistent-test-key ]]',
        '--',
        runtimePath.pathname,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          JULES_API_KEY: '',
          JULES_CREDENTIAL_HELPER: helperPath,
          JULES_LOCAL_RUNTIME_ROOT: join(directory, 'runtime'),
          JULES_LOCAL_TEST_MODE: 'true',
        },
      }
    )

    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout, '')
    assert.doesNotMatch(result.stderr, /persistent-test-key/)
  } finally {
    rmSync(directory, { force: true, recursive: true })
  }
})

test('audit and finalization paths survive the wrapper working directory', () => {
  const wrapper = script('jules.zsh')

  assert.match(wrapper, /audit_path="\$\{audit_path:A\}"/)
  assert.match(wrapper, /manifest_path="\$\{manifest_path:A\}"/)
  assert.match(wrapper, /audit-pr\)\n\s+require_key/)
})

test('Jules wrapper selects the newest artifact for status and cleanup', () => {
  const wrapper = script('jules.zsh')

  assert.match(wrapper, /local matches=\([^\n]+\(Nom\)\)/)
  assert.doesNotMatch(wrapper, /local matches=\([^\n]+\(NOm\)\)/)
})
