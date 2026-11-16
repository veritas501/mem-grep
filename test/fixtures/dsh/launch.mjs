// Launches the DeepSeek Harness source CLI with the task directory as its cwd.
//
// `node --import tsx/esm` resolves tsx from the cwd, but the headless runner records process.cwd()
// as the session cwd and refuses to resume a session from any other directory. This registers a
// resolver rooted in the harness repo, then runs the CLI in the task directory.
//
//   node launch.mjs <harness-repo> <workdir> -- <dsh args...>
import { pathToFileURL } from 'node:url'
import { spawn } from 'node:child_process'

const [harness, workdir, dash, ...args] = process.argv.slice(2)
if (!harness || !workdir || dash !== '--') {
  process.stderr.write('usage: node launch.mjs <harness-repo> <workdir> -- <dsh args...>\n')
  process.exit(2)
}
// tsx's ESM loader, addressed by file so it resolves no matter what the cwd is. The child inherits
// the caller's environment, which gen.sh keeps to PATH, HOME, DSH_HOME and the mock's endpoint.
const tsxLoader = pathToFileURL(`${harness}/node_modules/tsx/dist/esm/index.mjs`).href
const child = spawn(process.execPath, ['--import', tsxLoader, `${harness}/apps/cli/src/bin.ts`, ...args], {
  cwd: workdir,
  stdio: 'inherit',
  env: { ...process.env, TSX_TSCONFIG_PATH: `${harness}/tsconfig.json` },
})
child.on('exit', (code, signal) => process.exit(code ?? (signal ? 1 : 0)))
