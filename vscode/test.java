/**
quarkus
mvn io.quarkus:quarkus-maven-plugin:1.13.3.Final:create \
    -DprojectGroupId=cn.icgear.demo \
    -DprojectArtifactId=test \
    -DclassName="org.acme.getting.started.GreetingResource" \
    -Dpath="/hello"
cd test

javac test.java
java test
*/
public class test {

    public static void main(String[] args) {
        System.out.println("hello, world");
    }
}