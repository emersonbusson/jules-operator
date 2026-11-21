import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = fileURLToPath(new URL('../../', import.meta.url))
const schedulerPath = join(
  repoRoot,
  'tools/jules-mcp/local-scheduler.zsh'
)
const runnerPath = join(repoRoot, 'tools/jules-mcp/jules.zsh')
const installerPath = join(
  repoRoot,
  'tools/jules-mcp/install-local-hook.zsh'
)
const setupPath = join(repoRoot, 'tools/jules-mcp/setup-local.zsh')
const remoteWorkflowPath = join(
  repoRoot,
  '.github/workflows'
)

function withTempDir(callback) {
  const directory = mkdtempSync(join(tmpdir(), 'ramshared-jules-local-'))
  try {
    callback(directory)
  } finally {
    rmSync(directory, { force: true, recursive: true })
  }
}

test('scheduler Jules não possui workflow remoto', () => {
  assert.equal(
    existsSync(remoteWorkflowPath),
    false,
    'nenhum workflow deve executar o operador fora da máquina local'
  )
})

test('run-once executa daily-run local sem propagar a chave', () => {
  withTempDir((directory) => {
    const runnerPath = join(directory, 'fake-jules.zsh')
    const evidencePath = join(directory, 'runner-evidence.txt')
    writeFileSync(
      runnerPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "$1|${JULES_DAILY_AUTO_PR_ENABLED:-}|${JULES_DAILY_CANARY_PROMOTED:-}|${JULES_API_KEY:-absent}" >"$JULES_LOCAL_TEST_EVIDENCE"',
        '',
      ].join('\n')
    )
    chmodSync(runnerPath, 0o700)

    const result = spawnSync('zsh', [schedulerPath, 'run-once'], {
      encoding: 'utf8',
      env: {
        ...process.env,
        JULES_API_KEY: 'must-not-reach-daemon',
        JULES_LOCAL_RUNNER: runnerPath,
        JULES_LOCAL_RUNTIME_ROOT: join(directory, 'runtime'),
        JULES_LOCAL_TEST_EVIDENCE: evidencePath,
        JULES_LOCAL_TEST_MODE: 'true',
      },
    })

    assert.equal(result.status, 0, result.stderr)
    assert.equal(readFileSync(evidencePath, 'utf8').trim(), 'daily-run|true|false|absent')
    assert.doesNotMatch(`${result.stdout}${result.stderr}`, /must-not-reach-daemon/)
  })
})

test('watch-once reconcilia sessões sem abrir uma nova janela', () => {
  withTempDir((directory) => {
    const fakeRunnerPath = join(directory, 'fake-jules.zsh')
    const evidencePath = join(directory, 'watch-evidence.txt')
    writeFileSync(
      fakeRunnerPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "$1|${JULES_API_KEY:-absent}" >"$JULES_LOCAL_TEST_EVIDENCE"',
        '',
      ].join('\n')
    )
    chmodSync(fakeRunnerPath, 0o700)

    const result = spawnSync('zsh', [schedulerPath, 'watch-once'], {
      encoding: 'utf8',
      env: {
        ...process.env,
        JULES_API_KEY: 'must-not-reach-watch',
        JULES_LOCAL_RUNNER: fakeRunnerPath,
        JULES_LOCAL_RUNTIME_ROOT: join(directory, 'runtime'),
        JULES_LOCAL_TEST_EVIDENCE: evidencePath,
        JULES_LOCAL_TEST_MODE: 'true',
      },
    })

    assert.equal(result.status, 0, result.stderr)
    assert.equal(readFileSync(evidencePath, 'utf8').trim(), 'daily-watch|absent')
    assert.doesNotMatch(`${result.stdout}${result.stderr}`, /must-not-reach-watch/)
  })
})

test('loop local observa a cada cinco minutos e despacha por hora', () => {
  const scheduler = readFileSync(schedulerPath, 'utf8')
  const runner = readFileSync(runnerPath, 'utf8')

  assert.match(scheduler, /watch_interval=300/)
  assert.match(scheduler, /run_watch/)
  assert.match(runner, /daily-watch/)
})

test('scheduler de produção se desacopla da sessão que executou start', () => {
  const scheduler = readFileSync(schedulerPath, 'utf8')

  assert.match(scheduler, /command -v setsid/)
  assert.match(scheduler, /detach=\(setsid\)/)
  assert.match(scheduler, /nohup "\$\{detach\[@\]\}" zsh/)
  assert.match(scheduler, /run_once\(\) \{\n  \(/)
  assert.match(scheduler, /run_watch\(\) \{\n  \(/)
})

test('start é idempotente e stop encerra somente a instância verificada', () => {
  withTempDir((directory) => {
    const runnerPath = join(directory, 'fake-jules.zsh')
    const evidencePath = join(directory, 'runner-evidence.txt')
    const runtimeRoot = join(directory, 'runtime')
    writeFileSync(
      runnerPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "$$" >>"$JULES_LOCAL_TEST_EVIDENCE"',
        '',
      ].join('\n')
    )
    chmodSync(runnerPath, 0o700)
    const env = {
      ...process.env,
      JULES_API_KEY: 'must-not-reach-daemon',
      JULES_LOCAL_INTERVAL_SECONDS: '60',
      JULES_LOCAL_RUNNER: runnerPath,
      JULES_LOCAL_RUNTIME_ROOT: runtimeRoot,
      JULES_LOCAL_TEST_EVIDENCE: evidencePath,
      JULES_LOCAL_TEST_MODE: 'true',
    }

    const first = spawnSync('zsh', [schedulerPath, 'start'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(first.status, 0, first.stderr)
    const second = spawnSync('zsh', [schedulerPath, 'start'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(second.status, 0, second.stderr)

    const status = spawnSync('zsh', [schedulerPath, 'status'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(status.status, 0, status.stderr)
    assert.match(status.stdout, /ativo/)

    const stop = spawnSync('zsh', [schedulerPath, 'stop'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(stop.status, 0, stop.stderr)
    assert.doesNotMatch(
      existsSync(evidencePath) ? readFileSync(evidencePath, 'utf8') : '',
      /must-not-reach-daemon/
    )
  })
})

test('instalador do hook é idempotente e reversível', () => {
  withTempDir((directory) => {
    const zshrcPath = join(directory, '.zshrc')
    writeFileSync(zshrcPath, '# configuração preexistente\n')
    const env = {
      ...process.env,
      HOME: directory,
      ZDOTDIR: directory,
    }

    for (const action of ['install', 'install']) {
      const result = spawnSync('zsh', [installerPath, action], {
        encoding: 'utf8',
        env,
      })
      assert.equal(result.status, 0, result.stderr)
    }

    const installed = readFileSync(zshrcPath, 'utf8')
    assert.equal(
      installed.match(/RAMSHARED-JULES-LOCAL:BEGIN/g)?.length,
      1
    )
    assert.match(installed, /remote get-url origin/)
    assert.match(installed, /ramshared\/ramshared/)
    assert.match(installed, /local-scheduler\.zsh/)
    assert.match(installed, /add-zsh-hook chpwd/)

    const uninstall = spawnSync('zsh', [installerPath, 'uninstall'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(uninstall.status, 0, uninstall.stderr)
    assert.equal(
      readFileSync(zshrcPath, 'utf8'),
      '# configuração preexistente\n'
    )
  })
})

test('promoção do canário exige confirmação exata e pode ser revertida', () => {
  withTempDir((directory) => {
    const runnerPath = join(directory, 'fake-jules.zsh')
    const evidencePath = join(directory, 'runner-evidence.txt')
    writeFileSync(
      runnerPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "${JULES_DAILY_CANARY_PROMOTED:-}" >"$JULES_LOCAL_TEST_EVIDENCE"',
        '',
      ].join('\n')
    )
    chmodSync(runnerPath, 0o700)
    const env = {
      ...process.env,
      JULES_LOCAL_RUNNER: runnerPath,
      JULES_LOCAL_RUNTIME_ROOT: join(directory, 'runtime'),
      JULES_LOCAL_TEST_EVIDENCE: evidencePath,
      JULES_LOCAL_TEST_MODE: 'true',
    }

    const rejected = spawnSync('zsh', [schedulerPath, 'promote'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(rejected.status, 2)

    const promoted = spawnSync(
      'zsh',
      [schedulerPath, 'promote', '--confirm', '3/3'],
      { encoding: 'utf8', env }
    )
    assert.equal(promoted.status, 0, promoted.stderr)

    const runPromoted = spawnSync('zsh', [schedulerPath, 'run-once'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(runPromoted.status, 0, runPromoted.stderr)
    assert.equal(readFileSync(evidencePath, 'utf8').trim(), 'true')

    const demoted = spawnSync('zsh', [schedulerPath, 'demote'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(demoted.status, 0, demoted.stderr)

    const runDemoted = spawnSync('zsh', [schedulerPath, 'run-once'], {
      encoding: 'utf8',
      env,
    })
    assert.equal(runDemoted.status, 0, runDemoted.stderr)
    assert.equal(readFileSync(evidencePath, 'utf8').trim(), 'false')
  })
})

test('run-once atualiza origin/main e falha fechado antes do runner', () => {
  withTempDir((directory) => {
    const gitPath = join(directory, 'fake-git.zsh')
    const runnerPath = join(directory, 'fake-jules.zsh')
    const gitEvidencePath = join(directory, 'git-evidence.txt')
    const runnerEvidencePath = join(directory, 'runner-evidence.txt')
    writeFileSync(
      gitPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "$*" >"$JULES_LOCAL_GIT_EVIDENCE"',
        '[[ "${JULES_LOCAL_GIT_FAIL:-false}" != "true" ]]',
        '',
      ].join('\n')
    )
    writeFileSync(
      runnerPath,
      [
        '#!/usr/bin/env zsh',
        'print -r -- "$1" >"$JULES_LOCAL_TEST_EVIDENCE"',
        '',
      ].join('\n')
    )
    chmodSync(gitPath, 0o700)
    chmodSync(runnerPath, 0o700)
    const baseEnv = {
      ...process.env,
      JULES_LOCAL_GIT: gitPath,
      JULES_LOCAL_GIT_EVIDENCE: gitEvidencePath,
      JULES_LOCAL_RUNNER: runnerPath,
      JULES_LOCAL_RUNTIME_ROOT: join(directory, 'runtime'),
      JULES_LOCAL_TEST_EVIDENCE: runnerEvidencePath,
      JULES_LOCAL_TEST_MODE: 'true',
    }

    const passed = spawnSync('zsh', [schedulerPath, 'run-once'], {
      encoding: 'utf8',
      env: baseEnv,
    })
    assert.equal(passed.status, 0, passed.stderr)
    assert.match(
      readFileSync(gitEvidencePath, 'utf8'),
      /-C .* fetch --quiet origin main/
    )
    assert.equal(readFileSync(runnerEvidencePath, 'utf8').trim(), 'daily-run')

    rmSync(runnerEvidencePath)
    const failed = spawnSync('zsh', [schedulerPath, 'run-once'], {
      encoding: 'utf8',
      env: { ...baseEnv, JULES_LOCAL_GIT_FAIL: 'true' },
    })
    assert.equal(failed.status, 2)
    assert.equal(existsSync(runnerEvidencePath), false)
    assert.match(failed.stderr, /origin\/main/)
  })
})

test('setup único persiste a chave e ativa hook e scheduler', () => {
  withTempDir((directory) => {
    const fakeCommandPath = join(directory, 'fake-setup-command.zsh')
    const evidencePath = join(directory, 'setup-evidence.txt')
    const configRoot = join(directory, 'config')
    const ramsharedRepo = join(directory, 'ramshared')
    writeFileSync(
      fakeCommandPath,
      [
        '#!/usr/bin/env zsh',
        'if [[ "$1" == write ]]; then',
        '  IFS= read -r secret',
        '  print -r -- "store:$#:${#secret}" >>"$JULES_SETUP_TEST_EVIDENCE"',
        'elif (( $# == 0 )); then',
        '  print -r -- "publish" >>"$JULES_SETUP_TEST_EVIDENCE"',
        'else',
        '  print -r -- "$*" >>"$JULES_SETUP_TEST_EVIDENCE"',
        'fi',
        '',
      ].join('\n')
    )
    chmodSync(fakeCommandPath, 0o700)

    const result = spawnSync(
      'zsh',
      [setupPath, '--repo', ramsharedRepo],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          JULES_API_KEY: 'setup-test-key',
          JULES_CREDENTIAL_HELPER: fakeCommandPath,
          JULES_KEY_PUBLISHER: fakeCommandPath,
          JULES_LOCAL_HOOK_INSTALLER: fakeCommandPath,
          JULES_LOCAL_SCHEDULER: fakeCommandPath,
          JULES_LOCAL_TEST_MODE: 'true',
          JULES_SETUP_TEST_EVIDENCE: evidencePath,
          XDG_CONFIG_HOME: configRoot,
        },
      }
    )

    assert.equal(result.status, 0, result.stderr)
    assert.doesNotMatch(`${result.stdout}${result.stderr}`, /setup-test-key/)
    const evidence = readFileSync(evidencePath, 'utf8')
    assert.match(evidence, /store:1:14/)
    assert.match(evidence, /^install$/m)
    assert.match(evidence, new RegExp(`^start --repo ${ramsharedRepo}$`, 'm'))
    assert.match(evidence, /^publish$/m)

    const config = readFileSync(
      join(configRoot, 'ramshared', 'jules-operator', 'config'),
      'utf8'
    )
    assert.match(config, new RegExp(`^RAMSHARED_REPO=${ramsharedRepo}$`, 'm'))
    assert.doesNotMatch(config, /setup-test-key|JULES_API_KEY/)
  })
})
