/**
code-server --install-extension ms-vscode.cpptools  
code-server --install-extension ms-vscode.cmake-tools  

yum install -y cmake // make gcc gcc-c++ kernel-devel (已经安装)
g++ test.cpp -o test
 */
#include <iostream>
#include <string>
using namespace std;
int main(int argc, char const *argv[])
{
    cout<< "hello world" << endl;
    return 0;
}