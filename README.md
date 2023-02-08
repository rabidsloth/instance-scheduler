# instance-scheduler
### _essentially a lightswitch with a timer!_

## description
* controls startup/shutdown of non-prod ec2 instances
* allows instance owner to override scheduled shutdowns via slack
* datadog tracing available
* includes cat gifs!

## example config
```
billy.zane:
  name: remote-dev-billy-zane
  offdays: 
    - Saturday
    - Sunday
  offtime: 19h12m
  ontime: 7h
  override: false
  username: billy.zane
  email: billy.zane@coolperson.com
frank.sinatra:
...
```

## required env vars
* `AWS_ACCESS_KEY_ID`
* `AWS_SECRET_ACCESS_KEY`
* `AWS_DEFAULT_REGION`
* `SLACK_APP_TOKEN`
* `SLACK_BOT_TOKEN`
* `DD_AGENT_HOST`
* `DD_TRACE_AGENT_PORT`
* `DD_SERVICE`
* `DD_ENV`
* `GIPHY_API_KEY`
* `GIPHY_LIMIT`

## requirements

* IAM user with below permissions. Either access key/secret or IAM profile
* Slack app with app token and bot token
* Datadog config optional

### AWS permissions

```
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceStatus",
        "ec2:StartInstances",
        "ec2:StopInstances"
      ],
      "Resource": ['LIST_OF_INSTANCE_ARNS']
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances"
      ],
      "Resource": "*"
    }
  ]
}
```