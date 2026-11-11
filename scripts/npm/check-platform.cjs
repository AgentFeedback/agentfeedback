'use strict';

const supported = ['linux-x64', 'linux-arm64', 'darwin-x64', 'darwin-arm64', 'win32-x64', 'win32-arm64'];

function checkPlatform(platform = process.platform, arch = process.arch) {
  if (!supported.includes(`${platform}-${arch}`)) {
    throw new Error(`Unsupported platform ${platform}-${arch}. Supported platforms: ${supported.join(', ')}.`);
  }
}

module.exports = checkPlatform;

if (require.main === module) {
  try {
    checkPlatform();
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
