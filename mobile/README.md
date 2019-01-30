mobile
======

[![Build Status](https://travis-ci.org/gcash/bchwallet.png?branch=master)]
(https://travis-ci.org/gcash/bchwallet)

This package is intended to be used to run bchwallet on iOS and Android devices. It offers exported functions
which start and stop the wallet that can be called from the language binding.  

The following is an example of how to compile for Android:

1. Make sure the android SDK is installed on your computer. If you use android studio you should already have it installed.

2. Install NDK. You can find this in the Android SDK Manager inside Android Studio under SDK Tools.

3. Set the ANDROID_HOME environmental variable and also put it in your path:
    ```bash
    export ANDROID_HOME=$HOME/Android/Sdk
    export PATH=$PATH:$ANDROID_HOME/tools:$ANDROID_HOME/platforms-tools
    ```
4. Initalize gomobile
    ```
    gomobile init -ndk=$HOME/Android/Sdk/ndk-bundle
    ```
5. Build the language bindings. This generates a .aar file
    ```
    cd $GOPATH/src/github.com/gcash/bchwallet/mobile
    gomobile bind -target=android -o=bchwallet.aar
    ```
6. Import the .aar file as a dependency in Android Studio. See [this](https://stackoverflow.com/a/34919810) stack overflow answer for the steps
of how to import import into your project. 

7. You can now start the wallet from the Java code.
    ```java
    String configPath = getFilesDir() + "/bchwallet.conf";
    mobile.Mobile.StartWallet(configPath);
    ```
8. The wallet is now running and you can use the gRPC API to control it. 

9. Use `mobile.Mobile.StopWallet()` to stop it and perform a clean shutdown.

Note that `StartWallet` takes in a path to a config file. You will need to programatically create and save the config file on the device. As you do this make sure
to set the `appdata` and `logdir` to a valid path on the device. You will also most likely want to use the `noinitialload` config option and create the wallet using the API.

Finally you may run into [this](https://github.com/golang/go/issues/29706) bug in gomobile which may require you to modify a python file in your Android SDK.

Package mobile is licensed under the [copyfree](http://copyfree.org) ISC
License.
