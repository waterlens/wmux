import { rm } from 'node:fs/promises';
import { dataDir } from '../../playwright.config';

export default async function globalTeardown() {
  await rm(dataDir, { recursive: true, force: true });
}
