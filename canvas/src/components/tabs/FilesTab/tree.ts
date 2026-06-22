export interface FileEntry {
  path: string;
  size: number;
  dir: boolean;
}

export interface TreeNode {
  name: string;
  path: string;
  isDir: boolean;
  children: TreeNode[];
  size: number;
}

const FILE_ICONS: Record<string, string> = {
  ".md": "📄",
  ".yaml": "⚙",
  ".yml": "⚙",
  ".py": "🐍",
  ".ts": "💠",
  ".tsx": "💠",
  ".js": "📜",
  ".json": "{}",
  ".html": "🌐",
  ".css": "🎨",
  ".sh": "▸",
};

export function getIcon(path: string, isDir: boolean): string {
  if (isDir) return "📁";
  const ext = "." + (path.split(".").pop() ?? "").toLowerCase();
  return FILE_ICONS[ext] || "📄";
}

export function buildTree(files: FileEntry[]): TreeNode[] {
  const root: TreeNode[] = [];
  const dirMap = new Map<string, TreeNode>();

  // Sort: dirs first, then alphabetical
  const sorted = [...files].sort((a, b) => {
    if (a.dir !== b.dir) return a.dir ? -1 : 1;
    return a.path.localeCompare(b.path);
  });

  for (const file of sorted) {
    const parts = file.path.split("/");
    if (parts.length === 1) {
      // Check if already exists in dirMap (e.g. created by a nested child earlier)
      if (file.dir && dirMap.has(file.path)) continue;
      const node: TreeNode = { name: parts[0], path: file.path, isDir: file.dir, children: [], size: file.size };
      root.push(node);
      if (file.dir) dirMap.set(file.path, node);
    } else {
      // Find or create parent dirs
      let parentChildren = root;
      for (let i = 0; i < parts.length - 1; i++) {
        const dirPath = parts.slice(0, i + 1).join("/");
        let dirNode = dirMap.get(dirPath);
        if (!dirNode) {
          dirNode = { name: parts[i], path: dirPath, isDir: true, children: [], size: 0 };
          parentChildren.push(dirNode);
          dirMap.set(dirPath, dirNode);
        }
        parentChildren = dirNode.children;
      }
      if (file.dir) {
        const dirPath = file.path;
        if (!dirMap.has(dirPath)) {
          const dirNode: TreeNode = { name: parts[parts.length - 1], path: dirPath, isDir: true, children: [], size: 0 };
          parentChildren.push(dirNode);
          dirMap.set(dirPath, dirNode);
        }
      } else {
        parentChildren.push({
          name: parts[parts.length - 1],
          path: file.path,
          isDir: false,
          children: [],
          size: file.size,
        });
      }
    }
  }

  return root;
}
