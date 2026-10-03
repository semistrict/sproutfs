"""Pack the repository's source into a tarball for a disposable GCE node.

Only versioned and non-ignored files reach the node, including the Firecracker
submodule's: no credentials, Git internals or build caches. Every path is under
repo/ in the archive.

A checkout whose submodule is not initialised, such as a worktree, names a
clone of the Firecracker fork in SPROUTFS_FIRECRACKER_TREE instead. Its HEAD
must be the commit the repository pins, so the node never builds another VMM.

    python3 scripts/lib/stage-source.py REPOSITORY ARCHIVE.tar.gz
"""
import os
import pathlib
import subprocess
import sys
import tarfile

SUBMODULE = 'third_party/firecracker'


def git(directory, *args):
    return subprocess.check_output(['git', '-C', str(directory), *args])


def files(directory):
    return git(directory, 'ls-files', '--cached', '--others', '--exclude-standard', '-z').split(b'\0')


def firecracker_tree(root):
    """The checkout the submodule's files come from, or None for none."""
    submodule = root / SUBMODULE
    if (submodule / '.git').exists():
        return submodule
    named = os.environ.get('SPROUTFS_FIRECRACKER_TREE', '')
    if not named:
        return None
    tree = pathlib.Path(named)
    pinned = git(root, 'ls-tree', 'HEAD', SUBMODULE).split()[2].decode()
    head = git(tree, 'rev-parse', 'HEAD').decode().strip()
    if head != pinned:
        sys.exit(f'SPROUTFS_FIRECRACKER_TREE is at {head}, and the repository pins {pinned}')
    return tree


def main(root, archive_path):
    root = pathlib.Path(root)
    sources = {name.decode(): root / name.decode() for name in files(root) if name}
    tree = firecracker_tree(root)
    if tree is not None:
        sources.update((f'{SUBMODULE}/{name.decode()}', tree / name.decode()) for name in files(tree) if name)
    with tarfile.open(archive_path, 'w:gz') as archive:
        for name in sorted(sources):
            path = sources[name]
            if path.is_file() or path.is_symlink():
                archive.add(path, arcname='repo/' + name, recursive=False)


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
