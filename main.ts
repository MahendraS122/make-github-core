import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const binary = fileURLToPath(new URL('./bin/forge', import.meta.url));
const forge = spawn(binary, { env: process.env, stdio: 'inherit' });
forge.once('exit', () => process.exit(0));
forge.once('error', (error) => {
	console.error(error.message);
	process.exit(1);
});
