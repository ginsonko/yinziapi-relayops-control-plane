import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const ignoredDirectories = new Set(['.git', 'node_modules', 'dist']);
const forbiddenTopLevel = new Set(['.release', '.tmp', 'bin', 'data', 'tmp']);
const forbiddenExtensions = new Set([
  '.db', '.dll', '.exe', '.gz', '.key', '.log', '.p12', '.pem', '.pfx',
  '.sqlite', '.sqlite3', '.tar', '.zip',
]);
const maximumTrackedFileBytes = 1024 * 1024;

function walk(directory) {
  const files = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    if (ignoredDirectories.has(entry.name)) continue;
    const absolute = path.join(directory, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`symlink is not allowed in the public snapshot: ${path.relative(root, absolute)}`);
    if (entry.isDirectory()) files.push(...walk(absolute));
    else files.push(absolute);
  }
  return files;
}

const files = walk(root);

test('public tree excludes runtime, private deployment, binary, and generated artifacts', () => {
  for (const file of files) {
    const relative = path.relative(root, file).replaceAll('\\', '/');
    const top = relative.split('/')[0];
    assert.ok(!forbiddenTopLevel.has(top), `forbidden top-level path: ${relative}`);
    assert.ok(!relative.startsWith('deploy/yinzi/'), `private deployment path: ${relative}`);
    assert.notEqual(relative, 'compose.yinzi.yml');
    assert.ok(!forbiddenExtensions.has(path.extname(relative).toLowerCase()), `forbidden file type: ${relative}`);
    assert.ok(fs.statSync(file).size <= maximumTrackedFileBytes, `tracked file exceeds 1 MiB: ${relative}`);
  }
});

test('tracked text contains no common live-secret shapes', () => {
  const patterns = [
    /-----BEGIN (?:RSA |OPENSSH |EC )?PRIVATE KEY-----/,
    /\bgh[pousr]_[A-Za-z0-9]{20,}\b/,
    /\bsk-[A-Za-z0-9_-]{20,}\b/,
    /\bAKIA[0-9A-Z]{16}\b/,
  ];
  for (const file of files) {
    const buffer = fs.readFileSync(file);
    if (buffer.includes(0)) continue;
    const text = buffer.toString('utf8');
    for (const pattern of patterns) {
      assert.ok(!pattern.test(text), `${path.relative(root, file)} matches ${pattern}`);
    }
  }
});

test('module identity and public safety defaults stay explicit', () => {
  const goMod = fs.readFileSync(path.join(root, 'go.mod'), 'utf8');
  assert.match(goMod, /^module github\.com\/ginsonko\/yinziapi-relayops-control-plane$/m);
  const legacyModule = ['github.com', 'yinziapi', 'procurement-control-plane'].join('/');
  for (const file of files.filter((value) => /\.(?:go|mod|md)$/.test(value))) {
    assert.ok(!fs.readFileSync(file, 'utf8').includes(legacyModule), `legacy module in ${path.relative(root, file)}`);
  }

  const config = fs.readFileSync(path.join(root, 'internal/config/config.go'), 'utf8');
  assert.match(config, /minimumAdminTokenLength\s*=\s*32/);
  assert.match(config, /strictBoolEnv\("RELAYOPS_REMOTE_WRITES_ENABLED", false\)/);
  assert.match(config, /boolEnv\("RELAYOPS_AUTOMATION_ENABLED", false\)/);
  assert.match(config, /boolEnv\("RELAYOPS_BACKGROUND_WORKERS_ENABLED", false\)/);

  const compose = fs.readFileSync(path.join(root, 'compose.yml'), 'utf8');
  for (const name of ['RELAYOPS_REMOTE_WRITES_ENABLED', 'RELAYOPS_AUTOMATION_ENABLED', 'RELAYOPS_BACKGROUND_WORKERS_ENABLED']) {
    assert.match(compose, new RegExp(`${name}:.*:-false`));
  }
});

test('browser helper README version matches the extension manifest', () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'browser-extension/manifest.json'), 'utf8'));
  const readme = fs.readFileSync(path.join(root, 'browser-extension/README.md'), 'utf8');
  assert.match(readme, new RegExp(`v${manifest.version.replaceAll('.', '\\.')}(?:\\s|$)`));
});

test('relative Markdown links resolve inside the repository', () => {
  const markdownFiles = files.filter((file) => file.endsWith('.md'));
  const linkPattern = /!?\[[^\]]*\]\(([^)]+)\)/g;
  for (const file of markdownFiles) {
    const text = fs.readFileSync(file, 'utf8');
    for (const match of text.matchAll(linkPattern)) {
      let target = match[1].trim();
      if (target.startsWith('<') && target.endsWith('>')) target = target.slice(1, -1);
      target = target.split(/\s+['"]/)[0];
      if (!target || target.startsWith('#') || /^[a-z][a-z0-9+.-]*:/i.test(target)) continue;
      target = decodeURIComponent(target.split('#')[0]);
      const resolved = path.resolve(path.dirname(file), target);
      assert.ok(resolved === root || resolved.startsWith(`${root}${path.sep}`), `link escapes repository: ${target}`);
      assert.ok(fs.existsSync(resolved), `${path.relative(root, file)} has missing link: ${target}`);
    }
  }
});

test('HTTP API reference contains every registered API route', () => {
  const server = fs.readFileSync(path.join(root, 'internal/httpapi/server.go'), 'utf8');
  const reference = fs.readFileSync(path.join(root, 'docs/API_REFERENCE.md'), 'utf8');
  const routePattern = /(?:protected|api)\.(?:Get|Post|Put|Delete)\("([^"]+)"/g;
  const routes = new Set([...server.matchAll(routePattern)].map((match) => match[1]));
  assert.ok(routes.size > 100, `unexpectedly small route inventory: ${routes.size}`);
  for (const route of routes) {
    const documented = reference.includes(`\`${route}\``) || reference.includes(`\`/api/v1${route}\``);
    assert.ok(documented, `API reference is missing ${route}`);
  }
});
