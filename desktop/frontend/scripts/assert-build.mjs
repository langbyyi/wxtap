import { existsSync } from 'node:fs';
import { resolve } from 'node:path';

if (!existsSync(resolve(import.meta.dirname, '../dist/index.html'))) {
  throw new Error('dist/index.html was not generated');
}
