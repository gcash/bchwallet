# mobile

[![Build Status](https://github.com/gcash/bchwallet/actions/workflows/main.yml/badge.svg?branch=master)](https://github.com/gcash/bchwallet/actions/workflows/main.yml)

This package is intended to be used to run bchwallet on iOS and Android devices. It offers exported functions
which start and stop the wallet that can be called from the language binding.

The following is an example of how to compile for Android:

1. Install the Android SDK and the NDK, r28 or newer (r30 is tested). In Android Studio they are in the SDK Manager,
   the NDK under SDK Tools. NDK r28 and newer link the 64-bit libraries with the 16 KB page alignment that Google Play
   requires for apps targeting Android 15 and newer.

2. Point gomobile at them:
   ```bash
   export ANDROID_HOME=$HOME/Android/Sdk
   export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/30.0.16248370
   ```

3. Install gomobile and gobind at the version this module tracks as a tool. Run this from the bchwallet directory:
   ```bash
   go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
   ```

4. Build the language bindings. This generates a .aar file with libraries for arm64-v8a, armeabi-v7a, x86_64 and x86,
   or only the ones given with `-target`, such as `-target=android/arm64,android/arm`:
   ```bash
   gomobile bind -target=android -androidapi 24 -o bchwallet.aar ./mobile
   ```

5. Optionally check the page alignment of the 64-bit libraries. Each LOAD segment should be aligned to `2**14`:
   ```bash
   unzip -o bchwallet.aar 'jni/*' -d aar
   $ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/llvm-objdump -p aar/jni/arm64-v8a/libgojni.so | grep LOAD
   ```

6. Add the .aar file to your app, for example as a file dependency of the app module:
   ```groovy
   implementation files('libs/bchwallet.aar')
   ```

7. You can now start the wallet from the Java code.
   ```java
   String configPath = getFilesDir() + "/bchwallet.conf";
   mobile.Mobile.startWallet(configPath);
   ```
8. The wallet is now running and you can use the gRPC API to control it.

9. Use `mobile.Mobile.stopWallet()` to stop it and perform a clean shutdown. It returns once the wallet has closed its
   databases, so call it from a background thread. The wallet can then be started again in the same process.

`mobile.Mobile.isRunning()` tells whether the wallet is running, and `mobile.Mobile.lastError()` why it last failed to
start or stopped, such as an invalid config file or an RPC port that is in use.

Note that `startWallet` takes in a path to a config file. You will need to programatically create and save the config file on the device. As you do this make sure
to set the `appdata` and `logdir` to a valid path on the device. You will also most likely want to use the `noinitialload` config option and create the wallet using the API.
The wallet is configured only by this file: the command line of the app's process is not parsed, and the wallet does not
handle the process's signals.

Package mobile is licensed under the [copyfree](http://copyfree.org) ISC
License.
