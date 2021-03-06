# Copyright 2016 The Chromium Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.


# Recipe for Skia Infra.


import re


DEPS = [
  'depot_tools/bot_update',
  'depot_tools/gclient',
  'depot_tools/infra_paths',
  'depot_tools/rietveld',
  'recipe_engine/context',
  'recipe_engine/path',
  'recipe_engine/platform',
  'recipe_engine/properties',
  'recipe_engine/python',
  'recipe_engine/raw_io',
  'recipe_engine/shutil',
  'recipe_engine/step',
]


INFRA_GO = 'go.skia.org/infra'
INFRA_GIT_URL = 'https://skia.googlesource.com/buildbot'

REF_HEAD = 'HEAD'
REF_ORIGIN_MASTER = 'origin/master'


def git(api, *cmd, **kwargs):
  git_cmd = 'git.bat' if api.platform.is_win else 'git'
  return api.step(
      'git %s' % cmd[0],
      cmd=[git_cmd] + list(cmd),
      **kwargs)


def git_checkout(api, url, dest, ref=None):
  """Create a git checkout of the given repo in dest."""
  if api.path.exists(dest.join('.git')):
    # Already have a git checkout. Ensure that the correct remote is set.
    with api.context(cwd=dest):
      git(api, 'remote', 'set-url', 'origin', INFRA_GIT_URL)
  else:
    # Clone the repo
    git(api, 'clone', INFRA_GIT_URL, dest)

  # Ensure that the correct ref is checked out.
  ref = ref or REF_ORIGIN_MASTER
  if ref == REF_HEAD:
    ref = REF_ORIGIN_MASTER
  with api.context(cwd=dest):
    git(api, 'fetch', 'origin')
    git(api, 'clean', '-d', '-f')
    git(api, 'checkout', 'master')
    git(api, 'reset', '--hard', ref)

  api.path['checkout'] = dest

  # Run bot_update, just to apply patches.
  cfg_kwargs = {'CACHE_DIR': '/b/cache'}
  gclient_cfg = api.gclient.make_config(**cfg_kwargs)
  dirname = api.path['start_dir'].join('gopath', 'src', 'go.skia.org')
  basename = 'infra'
  sln = gclient_cfg.solutions.add()
  sln.name = basename
  sln.managed = False
  sln.url = INFRA_GIT_URL
  sln.revision = ref
  gclient_cfg.got_revision_mapping[basename] = 'got_revision'

  # Hack the patch_ref. Yay Python!
  if api.bot_update._issue and api.bot_update._patchset:
    api.bot_update._gerrit_ref = 'refs/changes/%s/%d/%d' % (
        str(api.bot_update._issue)[-2:],
        api.bot_update._issue,
        api.bot_update._patchset,
    )

  with api.context(cwd=dirname):
    api.bot_update.ensure_checkout(gclient_config=gclient_cfg)

  with api.context(cwd=dest):
    # Fix the remote URL, since bot_update switches it to the cached repo.
    git(api, 'remote', 'set-url', 'origin', INFRA_GIT_URL)

    # Re-checkout master, since bot_update detaches us. We already set master
    # to the correct commit, and any applied patch should not have been committed,
    # so this should be safe.
    git(api, 'checkout', 'master')

    # "git status" just to sanity check.
    git(api, 'status')


def RunSteps(api):
  # The 'build' and 'depot_tools directories come from recipe DEPS and aren't
  # provided by default. We have to set them manually.
  api.path.c.base_paths['depot_tools'] = (
      api.path.c.base_paths['start_dir'] +
      ('buildbot', 'infra', 'bots', '.recipe_deps', 'depot_tools'))

  go_dir = api.path['start_dir'].join('gopath')
  go_src = go_dir.join('src')
  api.shutil.makedirs('makedirs go/src', go_src)
  infra_dir = go_src.join(INFRA_GO)
  go_root = api.path['start_dir'].join('go', 'go')
  go_bin = go_root.join('bin')

  # Check out the infra repo.
  git_checkout(
      api,
      INFRA_GIT_URL,
      dest=infra_dir,
      ref=api.properties.get('revision', 'origin/master'))

  # Fetch Go dependencies.
  env = {'CHROME_HEADLESS': '1',
         'GOROOT': go_root,
         'GOPATH': go_dir,
         'GIT_USER_AGENT': 'git/1.9.1',  # I don't think this version matters.
         'PATH': api.path.pathsep.join([
             str(go_bin), str(go_dir.join('bin')), '%(PATH)s'])}
  with api.context(cwd=infra_dir, env=env):
    api.step('which go', cmd=['which', 'go'])
    api.step('update_deps', cmd=['go', 'get', '-u', '-t', './...'])

  # Checkout AGAIN to undo whatever `go get -u` did to the infra repo.
  git_checkout(
      api,
      INFRA_GIT_URL,
      dest=infra_dir,
      ref=api.properties.get('revision', 'origin/master'))

  # Set got_revision.
  test_data = lambda: api.raw_io.test_api.stream_output('abc123')
  with api.context(cwd=infra_dir):
    rev_parse = git(api, 'rev-parse', 'HEAD',
                    stdout=api.raw_io.output(),
                    step_test_data=test_data)
  rev_parse.presentation.properties['got_revision'] = rev_parse.stdout.strip()

  # More prerequisites.
  with api.context(cwd=infra_dir, env=env):
    api.step(
        'install goimports',
        cmd=['go', 'get', 'golang.org/x/tools/cmd/goimports'])
    api.step(
        'install errcheck',
        cmd=['go', 'get', 'github.com/kisielk/errcheck'])
  with api.context(cwd=infra_dir.join('go', 'database'), env=env):
    api.step(
        'setup database',
        cmd=['./setup_test_db'])

  # Run tests.
  buildslave = api.properties['slavename']
  m = re.search('^[a-zA-Z-]*(\d+)$', buildslave)
  karma_port = '9876'
  if m and len(m.groups()) > 0:
    karma_port = '15%s' % m.groups()[0]
  env['KARMA_PORT'] = karma_port
  env['DEPOT_TOOLS'] = api.path['depot_tools']
  env['TMPDIR'] = None
  env['PATH'] = api.path.pathsep.join([
      env['PATH'], str(api.path['depot_tools'])])

  cmd = ['go', 'run', './run_unittests.go', '--alsologtostderr']
  if 'Large' in api.properties['buildername']:
    cmd.append('--large')
  elif 'Medium' in api.properties['buildername']:
    cmd.append('--medium')
  else:
    cmd.append('--small')
  with api.context(cwd=infra_dir, env=env):
    api.step('run_unittests', cmd)


def GenTests(api):
  yield (
      api.test('Infra-PerCommit') +
      api.path.exists(api.path['start_dir'].join('gopath', 'src', INFRA_GO,
                                                 '.git')) +
      api.properties(buildername='Infra-PerCommit-Small',
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen')
  )
  yield (
      api.test('Infra-PerCommit_initialcheckout') +
      api.properties(buildername='Infra-PerCommit-Small',
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen')
  )
  yield (
      api.test('Infra-PerCommit_try_rietveld') +
      api.properties(buildername='Infra-PerCommit-Small',
                     rietveld='https://codereview.chromium.org',
                     issue=1234,
                     patchset=1,
                     revision=REF_HEAD,
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen')
  )
  yield (
      api.test('Infra-PerCommit_try_gerrit') +
      api.properties(buildername='Infra-PerCommit-Small',
                     revision=REF_HEAD,
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen') +
      api.properties.tryserver(
          gerrit_project='skia',
          gerrit_url='https://skia-review.googlesource.com/',
      )
  )
  yield (
      api.test('Infra-PerCommit-Large') +
      api.properties(buildername='Infra-PerCommit-Large',
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen')
  )
  yield (
      api.test('Infra-PerCommit-Medium') +
      api.properties(buildername='Infra-PerCommit-Medium',
                     slavename='skiabot-linux-infra-001',
                     path_config='kitchen')
  )
